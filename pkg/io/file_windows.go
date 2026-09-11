//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/microsoft/dcp/pkg/osutil"
	"golang.org/x/sys/windows"
)

type restrictedFileOpenMode uint8

const (
	restrictedFileCreateNew restrictedFileOpenMode = iota
	restrictedFileOpenOrCreate
	restrictedFileCreateOrTruncate
	restrictedFileWriteOrTruncate
	restrictedFileAppend

	ntFileOpened  = 1
	ntFileCreated = 2
)

type restrictedFileAccessEntry struct {
	sid  *windows.SID
	mask windows.ACCESS_MASK
}

func createNewFile(name string, perm os.FileMode) (*os.File, error) {
	return openFile(name, restrictedFileCreateNew, perm)
}

func ensureFile(name string, perm os.FileMode) (*os.File, error) {
	return openFile(name, restrictedFileOpenOrCreate, perm)
}

func ensureEmptyFile(name string, perm os.FileMode) (*os.File, error) {
	return openFile(name, restrictedFileCreateOrTruncate, perm)
}

func ensureEmptyFileForWriting(name string, perm os.FileMode) (*os.File, error) {
	return openFile(name, restrictedFileWriteOrTruncate, perm)
}

func openOrCreateFileForAppending(name string, perm os.FileMode) (*AppendFile, error) {
	file, openErr := openFile(name, restrictedFileAppend, perm)
	if openErr != nil {
		return nil, openErr
	}
	return newAppendFile(file), nil
}

func openFile(name string, mode restrictedFileOpenMode, perm os.FileMode) (*os.File, error) {
	isElevated, elevationErr := osutil.IsAdmin()
	if elevationErr != nil {
		return nil, elevationErr
	}
	if !isElevated {
		return os.OpenFile(name, standardFileFlags(mode), perm)
	}

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	if principalErr != nil {
		return nil, principalErr
	}
	return openRestrictedFile(name, mode, perm, principals)
}

func standardFileFlags(mode restrictedFileOpenMode) int {
	switch mode {
	case restrictedFileCreateNew:
		return os.O_RDWR | os.O_CREATE | os.O_EXCL
	case restrictedFileOpenOrCreate:
		return os.O_RDWR | os.O_CREATE
	case restrictedFileCreateOrTruncate:
		return os.O_RDWR | os.O_CREATE | os.O_TRUNC
	case restrictedFileWriteOrTruncate:
		return os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	case restrictedFileAppend:
		return os.O_WRONLY | os.O_CREATE | os.O_APPEND
	default:
		panic(fmt.Sprintf("unsupported file open mode %d", mode))
	}
}

func openRestrictedFile(
	name string,
	mode restrictedFileOpenMode,
	perm os.FileMode,
	principals restrictedDirectoryPrincipals,
) (*os.File, error) {
	if !filepath.IsAbs(name) {
		return nil, fmt.Errorf(
			"%w: %w: restricted files require an absolute path on a fixed local drive: %q",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileUnsupportedPath,
			name,
		)
	}
	absoluteName := filepath.Clean(name)
	volumeName := filepath.VolumeName(absoluteName)
	if len(volumeName) != 2 || volumeName[1] != ':' {
		return nil, fmt.Errorf(
			"%w: %w: restricted files require an absolute path on a fixed local drive: %q",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileUnsupportedPath,
			name,
		)
	}
	if strings.Contains(strings.TrimPrefix(absoluteName, volumeName), ":") {
		return nil, fmt.Errorf(
			"%w: %w: alternate data streams are not supported: %q",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileUnsupportedPath,
			name,
		)
	}
	if componentErr := validateRestrictedFilePathComponents(
		strings.TrimPrefix(absoluteName, volumeName+`\`),
	); componentErr != nil {
		return nil, fmt.Errorf(
			"%w: %w: invalid restricted file path %q: %w",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileUnsupportedPath,
			name,
			componentErr,
		)
	}
	driveRoot, driveRootErr := windows.UTF16PtrFromString(volumeName + `\`)
	if driveRootErr != nil {
		return nil, fmt.Errorf("creating drive root path %q: %w", volumeName, driveRootErr)
	}
	if driveType := windows.GetDriveType(driveRoot); driveType != windows.DRIVE_FIXED {
		return nil, fmt.Errorf(
			"%w: %w: restricted files require a fixed local drive, got drive type %d: %q",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileNonFixedDrive,
			driveType,
			name,
		)
	}
	var fileSystemFlags uint32
	if volumeErr := windows.GetVolumeInformation(
		driveRoot,
		nil,
		0,
		nil,
		nil,
		&fileSystemFlags,
		nil,
		0,
	); volumeErr != nil {
		return nil, fmt.Errorf("getting file system capabilities for %q: %w", volumeName, volumeErr)
	}
	if fileSystemFlags&windows.FILE_PERSISTENT_ACLS == 0 {
		return nil, fmt.Errorf(
			"%w: %w: restricted files require a file system with persistent ACLs: %q",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileNoPersistentACLs,
			name,
		)
	}

	objectName, objectNameErr := windows.NewNTUnicodeString(`\??\` + absoluteName)
	if objectNameErr != nil {
		return nil, fmt.Errorf("creating native file path %q: %w", name, objectNameErr)
	}
	securityDescriptor, securityDescriptorErr := restrictedFileSecurityDescriptor(principals, perm)
	if securityDescriptorErr != nil {
		return nil, securityDescriptorErr
	}

	objectAttributes := windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: securityDescriptor,
	}
	var ioStatus windows.IO_STATUS_BLOCK
	var handle windows.Handle
	openErr := windows.NtCreateFile(
		&handle,
		restrictedFileAccess(mode),
		&objectAttributes,
		&ioStatus,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		restrictedFileDisposition(mode),
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	runtime.KeepAlive(objectName)
	runtime.KeepAlive(securityDescriptor)
	runtime.KeepAlive(principals)
	if openErr != nil {
		return nil, restrictedFileOpenError(name, openErr)
	}

	closeHandle := true
	defer func() {
		if closeHandle {
			_ = windows.CloseHandle(handle)
		}
	}()

	switch ioStatus.Information {
	case ntFileOpened, ntFileCreated:
	default:
		return nil, fmt.Errorf("opening restricted file %q returned unexpected status %d", name, ioStatus.Information)
	}

	if validationErr := validateRestrictedFile(handle, perm, principals); validationErr != nil {
		validationKind := ErrRestrictedFileInvalidSecurity
		if errors.Is(validationErr, ErrRestrictedFileReparsePoint) {
			validationKind = ErrRestrictedFileReparsePoint
		}
		return nil, fmt.Errorf(
			"%w: %w: validating restricted file %q: %w",
			ErrRestrictedFilePolicy,
			validationKind,
			name,
			validationErr,
		)
	}
	if mode == restrictedFileCreateOrTruncate || mode == restrictedFileWriteOrTruncate {
		if truncateErr := windows.Ftruncate(handle, 0); truncateErr != nil {
			return nil, fmt.Errorf("truncating restricted file %q: %w", name, truncateErr)
		}
	}

	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		return nil, fmt.Errorf("creating Go file for restricted file %q", name)
	}
	closeHandle = false
	return file, nil
}

func validateRestrictedFilePathComponents(relativePath string) error {
	for _, component := range strings.Split(relativePath, `\`) {
		if component == "" {
			return fmt.Errorf("path contains an empty component")
		}
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("path component %q ends with a space or period", component)
		}
		if strings.ContainsAny(component, `<>:"/|?*`) {
			return fmt.Errorf("path component %q contains a reserved character", component)
		}
		for _, character := range component {
			if character < 32 {
				return fmt.Errorf("path component %q contains a control character", component)
			}
		}

		baseName := component
		if extensionIndex := strings.IndexByte(baseName, '.'); extensionIndex >= 0 {
			baseName = baseName[:extensionIndex]
		}
		switch strings.ToUpper(baseName) {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
			"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
			"COM\u00B9", "COM\u00B2", "COM\u00B3", "LPT\u00B9", "LPT\u00B2", "LPT\u00B3":
			return fmt.Errorf("path component %q uses a reserved device name", component)
		}
	}
	return nil
}

func restrictedFileAccess(mode restrictedFileOpenMode) uint32 {
	access := uint32(windows.READ_CONTROL | windows.SYNCHRONIZE)
	if mode == restrictedFileWriteOrTruncate {
		return access | windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES
	}
	if mode == restrictedFileAppend {
		return access |
			windows.FILE_APPEND_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.FILE_WRITE_ATTRIBUTES |
			windows.FILE_WRITE_EA |
			windows.STANDARD_RIGHTS_WRITE
	}
	return access | windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
}

func restrictedFileDisposition(mode restrictedFileOpenMode) uint32 {
	if mode == restrictedFileCreateNew {
		return windows.FILE_CREATE
	}
	return windows.FILE_OPEN_IF
}

func restrictedFileSecurityDescriptor(
	principals restrictedDirectoryPrincipals,
	perm os.FileMode,
) (*windows.SECURITY_DESCRIPTOR, error) {
	entries := restrictedFileAccessEntries(principals, perm)
	explicitEntries := make([]windows.EXPLICIT_ACCESS, 0, len(entries))
	for _, entry := range entries {
		explicitEntries = append(explicitEntries, windows.EXPLICIT_ACCESS{
			AccessPermissions: entry.mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(entry.sid),
			},
		})
	}

	acl, aclErr := windows.ACLFromEntries(explicitEntries, nil)
	if aclErr != nil {
		return nil, fmt.Errorf("creating restricted file dacl: %w", aclErr)
	}
	securityDescriptor, descriptorErr := windows.NewSecurityDescriptor()
	if descriptorErr != nil {
		return nil, fmt.Errorf("creating restricted file security descriptor: %w", descriptorErr)
	}
	if ownerErr := securityDescriptor.SetOwner(principals.admins, false); ownerErr != nil {
		return nil, fmt.Errorf("setting restricted file owner: %w", ownerErr)
	}
	if daclErr := securityDescriptor.SetDACL(acl, true, false); daclErr != nil {
		return nil, fmt.Errorf("setting restricted file dacl: %w", daclErr)
	}
	if controlErr := securityDescriptor.SetControl(
		windows.SE_DACL_PROTECTED,
		windows.SE_DACL_PROTECTED,
	); controlErr != nil {
		return nil, fmt.Errorf("protecting restricted file dacl: %w", controlErr)
	}
	return securityDescriptor, nil
}

func restrictedFileAccessEntries(
	principals restrictedDirectoryPrincipals,
	perm os.FileMode,
) []restrictedFileAccessEntry {
	var entries []restrictedFileAccessEntry
	addEntry := func(sid *windows.SID, mask windows.ACCESS_MASK) {
		for entryIndex := range entries {
			if windows.EqualSid(entries[entryIndex].sid, sid) {
				entries[entryIndex].mask |= mask
				return
			}
		}
		entries = append(entries, restrictedFileAccessEntry{sid: sid, mask: mask})
	}

	addEntry(principals.tokenUser, restrictedFileUserAccess(perm))
	const fileSpecificRightsAll = 0x1ff
	fullAccess := windows.ACCESS_MASK(
		windows.STANDARD_RIGHTS_REQUIRED |
			windows.SYNCHRONIZE |
			fileSpecificRightsAll,
	)
	addEntry(principals.system, fullAccess)
	addEntry(principals.admins, fullAccess)
	return entries
}

func restrictedFileUserAccess(perm os.FileMode) windows.ACCESS_MASK {
	access := windows.ACCESS_MASK(
		windows.READ_CONTROL |
			windows.DELETE |
			windows.FILE_READ_ATTRIBUTES |
			windows.FILE_READ_EA,
	)
	if perm&osutil.PermissionCheckEveryoneRead != 0 {
		access |= windows.FILE_GENERIC_READ
	}
	if perm&osutil.PermissionCheckEveryoneWrite != 0 {
		access |= windows.FILE_GENERIC_WRITE
	}
	if perm&osutil.PermissionCheckEveryoneExecute != 0 {
		access |= windows.FILE_GENERIC_EXECUTE
	}
	return access
}

func validateRestrictedFile(
	handle windows.Handle,
	perm os.FileMode,
	principals restrictedDirectoryPrincipals,
) error {
	var fileInfo windows.ByHandleFileInformation
	if infoErr := windows.GetFileInformationByHandle(handle, &fileInfo); infoErr != nil {
		return fmt.Errorf("getting file information: %w", infoErr)
	}
	if fileInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%w: file is a reparse point", ErrRestrictedFileReparsePoint)
	}
	if fileInfo.NumberOfLinks != 1 {
		return fmt.Errorf("file has %d hard links", fileInfo.NumberOfLinks)
	}

	securityDescriptor, securityDescriptorErr := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if securityDescriptorErr != nil {
		return fmt.Errorf("getting file security descriptor: %w", securityDescriptorErr)
	}
	owner, _, ownerErr := securityDescriptor.Owner()
	if ownerErr != nil {
		return fmt.Errorf("getting file owner: %w", ownerErr)
	}
	if !windows.EqualSid(owner, principals.admins) {
		return fmt.Errorf("file owner is not the administrators group")
	}
	control, _, controlErr := securityDescriptor.Control()
	if controlErr != nil {
		return fmt.Errorf("getting file security descriptor control: %w", controlErr)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("file dacl is not protected")
	}

	dacl, _, daclErr := securityDescriptor.DACL()
	if daclErr != nil {
		return fmt.Errorf("getting file dacl: %w", daclErr)
	}
	if dacl == nil {
		return fmt.Errorf("file dacl is empty")
	}

	expectedEntries := restrictedFileAccessEntries(principals, perm)
	if int(dacl.AceCount) != len(expectedEntries) {
		return fmt.Errorf("file dacl contains %d entries, expected %d", dacl.AceCount, len(expectedEntries))
	}
	seenEntries := make([]bool, len(expectedEntries))
	for aceIndex := uint16(0); aceIndex < dacl.AceCount; aceIndex++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if aceErr := windows.GetAce(dacl, uint32(aceIndex), &ace); aceErr != nil {
			return fmt.Errorf("getting file dacl entry %d: %w", aceIndex, aceErr)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("file dacl entry %d has unsupported type %d", aceIndex, ace.Header.AceType)
		}
		if ace.Header.AceFlags != windows.NO_INHERITANCE {
			return fmt.Errorf("file dacl entry %d is inheritable", aceIndex)
		}

		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := false
		for expectedIndex, expectedEntry := range expectedEntries {
			if windows.EqualSid(aceSID, expectedEntry.sid) {
				if seenEntries[expectedIndex] {
					return fmt.Errorf("file dacl contains duplicate entry for %s", aceSID.String())
				}
				if ace.Mask != expectedEntry.mask {
					return fmt.Errorf(
						"file dacl entry for %s has access %#x, expected %#x",
						aceSID.String(),
						ace.Mask,
						expectedEntry.mask,
					)
				}
				seenEntries[expectedIndex] = true
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("file dacl grants access to unexpected principal %s", aceSID.String())
		}
	}
	for expectedIndex, seen := range seenEntries {
		if !seen {
			return fmt.Errorf("file dacl is missing entry for %s", expectedEntries[expectedIndex].sid.String())
		}
	}
	return nil
}

func restrictedFileOpenError(name string, openErr error) error {
	var ntStatus windows.NTStatus
	if errors.As(openErr, &ntStatus) {
		openErr = ntStatus.Errno()
	}
	pathErr := &os.PathError{Op: "open", Path: name, Err: openErr}
	if errors.Is(openErr, windows.ERROR_REPARSE_POINT_ENCOUNTERED) ||
		errors.Is(openErr, windows.ERROR_CANT_ACCESS_FILE) {
		return fmt.Errorf(
			"%w: %w: restricted file path traverses a reparse point: %w",
			ErrRestrictedFilePolicy,
			ErrRestrictedFileReparsePoint,
			pathErr,
		)
	}
	return pathErr
}
