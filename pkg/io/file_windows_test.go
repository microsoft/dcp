//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/osutil"
)

func TestRestrictedFileWritableAccessDoesNotRequestWriteDAC(t *testing.T) {
	t.Parallel()

	for _, mode := range []restrictedFileOpenMode{
		restrictedFileCreateNew,
		restrictedFileOpenOrCreate,
		restrictedFileCreateOrTruncate,
		restrictedFileWriteOrTruncate,
		restrictedFileAppend,
	} {
		require.Zero(t, restrictedFileAccess(mode)&uint32(windows.WRITE_DAC))
	}
}

func TestRestrictedFileRejectsUntrustedExistingFile(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "untrusted.txt")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0600))

	file, openErr := openRestrictedFile(
		path,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileInvalidSecurity)

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "existing", string(contents))
}

func TestRestrictedFileCreateRejectsExistingFile(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "existing.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	require.NoError(t, file.Close())

	secondFile, secondCreateErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if secondFile != nil {
		require.NoError(t, secondFile.Close())
	}
	require.ErrorIs(t, secondCreateErr, os.ErrExist)
}

func TestRestrictedFileRejectsReparsePathBeforeTruncate(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "target.txt")
	linkPath := filepath.Join(tempDir, "link.txt")
	require.NoError(t, os.WriteFile(targetPath, []byte("existing"), 0600))
	if symlinkErr := os.Symlink(targetPath, linkPath); symlinkErr != nil {
		t.Skipf("symlink creation is unavailable: %v", symlinkErr)
	}

	file, openErr := openRestrictedFile(
		linkPath,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileReparsePoint)

	contents, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr)
	require.Equal(t, "existing", string(contents))
}

func TestRestrictedFileRejectsAncestorReparsePoint(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "target")
	require.NoError(t, os.Mkdir(targetDir, 0700))
	linkDir := filepath.Join(tempDir, "link")
	if symlinkErr := os.Symlink(targetDir, linkDir); symlinkErr != nil {
		t.Skipf("symlink creation is unavailable: %v", symlinkErr)
	}

	file, openErr := openRestrictedFile(
		filepath.Join(linkDir, "created.txt"),
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileReparsePoint)
	require.NoFileExists(t, filepath.Join(targetDir, "created.txt"))
}

func TestRestrictedFileRejectsHardLinks(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	targetPath := filepath.Join(t.TempDir(), "target.txt")
	target, targetErr := openRestrictedFile(
		targetPath,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, targetErr)
	require.NoError(t, target.Close())

	linkPath := filepath.Join(filepath.Dir(targetPath), "link.txt")
	require.NoError(t, os.Link(targetPath, linkPath))
	file, openErr := openRestrictedFile(
		linkPath,
		restrictedFileAppend,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileInvalidSecurity)
	require.ErrorContains(t, openErr, "hard links")
}

func TestRestrictedFileRejectsUnsupportedPathForms(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	testCases := []struct {
		name string
		path string
	}{
		{name: "relative path", path: "file.txt"},
		{name: "UNC path", path: `\\server\share\file.txt`},
		{name: "extended path", path: `\\?\C:\file.txt`},
		{name: "alternate data stream", path: filepath.Join(t.TempDir(), "file.txt:stream")},
		{name: "trailing period", path: filepath.Join(t.TempDir(), "file.txt.")},
		{name: "trailing space", path: filepath.Join(t.TempDir(), "file.txt ")},
		{name: "reserved device", path: filepath.Join(t.TempDir(), "NUL.txt")},
		{name: "superscript reserved device", path: filepath.Join(t.TempDir(), "COM\u00B9.txt")},
		{name: "wildcard", path: filepath.Join(t.TempDir(), "file?.txt")},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			file, openErr := openRestrictedFile(
				testCase.path,
				restrictedFileCreateNew,
				osutil.PermissionOnlyOwnerReadWrite,
				principals,
			)
			if file != nil {
				require.NoError(t, file.Close())
			}
			require.ErrorIs(t, openErr, ErrRestrictedFileUnsupportedPath)
		})
	}
}

func TestRestrictedFileAccessEntriesUseDistinctPrincipals(t *testing.T) {
	t.Parallel()

	principals := syntheticRestrictedFilePrincipals(t)
	testCases := []struct {
		name string
		perm os.FileMode
	}{
		{name: "no delegated data access", perm: 0},
		{name: "read", perm: osutil.PermissionCheckEveryoneRead},
		{name: "write", perm: osutil.PermissionCheckEveryoneWrite},
		{name: "execute", perm: osutil.PermissionCheckEveryoneExecute},
		{
			name: "read write execute",
			perm: osutil.PermissionCheckEveryoneRead |
				osutil.PermissionCheckEveryoneWrite |
				osutil.PermissionCheckEveryoneExecute,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			entries := restrictedFileAccessEntries(principals, testCase.perm)

			require.Len(t, entries, 3)
			require.Equal(t, expectedRestrictedFileUserAccess(testCase.perm), accessMaskForSID(t, entries, principals.tokenUser))
			require.Equal(t, restrictedFileFullAccess(), accessMaskForSID(t, entries, principals.system))
			require.Equal(t, restrictedFileFullAccess(), accessMaskForSID(t, entries, principals.admins))
		})
	}
}

func TestRestrictedFileSecurityDescriptorHasCanonicalACL(t *testing.T) {
	t.Parallel()

	principals := syntheticRestrictedFilePrincipals(t)
	perm := osutil.PermissionCheckEveryoneRead | osutil.PermissionCheckEveryoneWrite
	securityDescriptor, descriptorErr := restrictedFileSecurityDescriptor(principals, perm)
	require.NoError(t, descriptorErr)

	owner, _, ownerErr := securityDescriptor.Owner()
	require.NoError(t, ownerErr)
	require.True(t, windows.EqualSid(principals.admins, owner))

	control, _, controlErr := securityDescriptor.Control()
	require.NoError(t, controlErr)
	require.NotZero(t, control&windows.SE_DACL_PROTECTED)

	dacl, _, daclErr := securityDescriptor.DACL()
	require.NoError(t, daclErr)
	require.NotNil(t, dacl)
	require.Equal(t, uint16(3), dacl.AceCount)

	expectedMasks := map[string]windows.ACCESS_MASK{
		principals.tokenUser.String(): expectedRestrictedFileUserAccess(perm),
		principals.system.String():    restrictedFileFullAccess(),
		principals.admins.String():    restrictedFileFullAccess(),
	}
	for aceIndex := uint16(0); aceIndex < dacl.AceCount; aceIndex++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(t, windows.GetAce(dacl, uint32(aceIndex), &ace))
		require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
		require.Equal(t, uint8(windows.NO_INHERITANCE), ace.Header.AceFlags)

		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		expectedMask, found := expectedMasks[aceSID.String()]
		require.True(t, found, "unexpected principal %s", aceSID.String())
		require.Equal(t, expectedMask, ace.Mask)
		delete(expectedMasks, aceSID.String())
	}
	require.Empty(t, expectedMasks)
}

func TestRestrictedFileAppendUsesNativeAppendAccess(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "append.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	_, writeErr := file.WriteString("first")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	appendFile, appendErr := openRestrictedFile(
		path,
		restrictedFileAppend,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, appendErr)
	_, seekErr := appendFile.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)
	_, appendWriteErr := appendFile.WriteString("-second")
	require.NoError(t, appendWriteErr)
	require.NoError(t, appendFile.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "first-second", string(contents))
}

func TestRestrictedFileValidatesBeforeTruncate(t *testing.T) {
	principals := restrictedTestPrincipals(t)
	path := filepath.Join(t.TempDir(), "truncate.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	_, writeErr := file.WriteString("existing")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	truncated, truncateErr := openRestrictedFile(
		path,
		restrictedFileCreateOrTruncate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, truncateErr)
	require.NoError(t, truncated.Close())

	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Empty(t, contents)
}

func TestRestrictedFileCreatesCanonicalDescriptorWithRealPrincipals(t *testing.T) {
	requireElevatedWindowsTest(t)

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	require.NoError(t, principalErr)
	path := filepath.Join(t.TempDir(), "real-principals.txt")
	file, createErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, createErr)
	require.NoError(t, validateRestrictedFile(
		windows.Handle(file.Fd()),
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	))
	require.NoError(t, file.Close())
}

func TestRestrictedFileOpensLegacyDescriptorAndMapsGenericMasks(t *testing.T) {
	requireElevatedWindowsTest(t)

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	require.NoError(t, principalErr)
	path := filepath.Join(t.TempDir(), "legacy.txt")
	createLegacyRestrictedFile(t, path, osutil.PermissionOnlyOwnerReadWrite, principals)

	securityDescriptor, descriptorErr := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	require.NoError(t, descriptorErr)
	owner, _, ownerErr := securityDescriptor.Owner()
	require.NoError(t, ownerErr)
	t.Logf("legacy descriptor owner: %s", owner.String())

	dacl, _, daclErr := securityDescriptor.DACL()
	require.NoError(t, daclErr)
	require.NotNil(t, dacl)
	entries := accessEntriesFromACL(t, dacl)
	require.Equal(t, restrictedFileFullAccess(), accessMaskForSID(t, entries, principals.system))
	require.Equal(t, restrictedFileFullAccess(), accessMaskForSID(t, entries, principals.admins))

	file, openErr := openRestrictedFile(
		path,
		restrictedFileOpenOrCreate,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	require.NoError(t, openErr, "legacy descriptor owner %s was not accepted", owner.String())
	require.NoError(t, file.Close())
}

func TestRestrictedFileRejectsConfiguredMappedDrive(t *testing.T) {
	root := os.Getenv("DCP_TEST_RESTRICTED_FILE_MAPPED_DRIVE_ROOT")
	if root == "" {
		t.Skip("DCP_TEST_RESTRICTED_FILE_MAPPED_DRIVE_ROOT is not configured")
	}

	principals := restrictedTestPrincipals(t)
	file, openErr := openRestrictedFile(
		filepath.Join(root, "dcp-restricted-file-test.txt"),
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileNonFixedDrive)
}

func TestRestrictedFileRejectsConfiguredACLlessStorage(t *testing.T) {
	root := os.Getenv("DCP_TEST_RESTRICTED_FILE_ACLLESS_ROOT")
	if root == "" {
		t.Skip("DCP_TEST_RESTRICTED_FILE_ACLLESS_ROOT is not configured")
	}

	principals := restrictedTestPrincipals(t)
	file, openErr := openRestrictedFile(
		filepath.Join(root, "dcp-restricted-file-test.txt"),
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if file != nil {
		require.NoError(t, file.Close())
	}
	require.ErrorIs(t, openErr, ErrRestrictedFileNoPersistentACLs)
}

func TestRestrictedFileCharacterizesSubstDrive(t *testing.T) {
	if os.Getenv("DCP_TEST_ENABLE_SUBST") != "true" {
		t.Skip("DCP_TEST_ENABLE_SUBST is not enabled")
	}

	drive := unusedDriveLetter(t)
	substErr := exec.Command("subst", drive, t.TempDir()).Run()
	require.NoError(t, substErr)
	t.Cleanup(func() {
		require.NoError(t, exec.Command("subst", drive, "/d").Run())
	})

	principals := restrictedTestPrincipals(t)
	path := filepath.Join(drive+`\`, "dcp-restricted-file-test.txt")
	file, openErr := openRestrictedFile(
		path,
		restrictedFileCreateNew,
		osutil.PermissionOnlyOwnerReadWrite,
		principals,
	)
	if openErr != nil {
		require.ErrorIs(t, openErr, ErrRestrictedFilePolicy)
		require.True(
			t,
			errors.Is(openErr, ErrRestrictedFileReparsePoint) ||
				errors.Is(openErr, ErrRestrictedFileNonFixedDrive),
			"unexpected SUBST rejection: %v",
			openErr,
		)
		t.Logf("restricted-file policy rejected SUBST destination %s: %v", path, openErr)
		return
	}
	require.NoError(t, file.Close())
	t.Logf("restricted-file policy accepted SUBST destination %s", path)
}

func restrictedTestPrincipals(t *testing.T) restrictedDirectoryPrincipals {
	t.Helper()

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	require.NoError(t, principalErr)
	principals.admins = principals.tokenUser
	return principals
}

func syntheticRestrictedFilePrincipals(t *testing.T) restrictedDirectoryPrincipals {
	t.Helper()

	tokenUser, tokenUserErr := windows.StringToSid("S-1-5-21-1000-1000-1000-1001")
	require.NoError(t, tokenUserErr)
	system, systemErr := windows.StringToSid("S-1-5-18")
	require.NoError(t, systemErr)
	admins, adminsErr := windows.StringToSid("S-1-5-32-544")
	require.NoError(t, adminsErr)
	return restrictedDirectoryPrincipals{
		tokenUser: tokenUser,
		system:    system,
		admins:    admins,
	}
}

func expectedRestrictedFileUserAccess(perm os.FileMode) windows.ACCESS_MASK {
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

func restrictedFileFullAccess() windows.ACCESS_MASK {
	const fileSpecificRightsAll = 0x1ff
	return windows.ACCESS_MASK(
		windows.STANDARD_RIGHTS_REQUIRED |
			windows.SYNCHRONIZE |
			fileSpecificRightsAll,
	)
}

func accessMaskForSID(
	t *testing.T,
	entries []restrictedFileAccessEntry,
	sid *windows.SID,
) windows.ACCESS_MASK {
	t.Helper()
	for _, entry := range entries {
		if windows.EqualSid(entry.sid, sid) {
			return entry.mask
		}
	}
	t.Fatalf("no access entry for %s", sid.String())
	return 0
}

func accessEntriesFromACL(t *testing.T, dacl *windows.ACL) []restrictedFileAccessEntry {
	t.Helper()

	entries := make([]restrictedFileAccessEntry, 0, dacl.AceCount)
	for aceIndex := uint16(0); aceIndex < dacl.AceCount; aceIndex++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(t, windows.GetAce(dacl, uint32(aceIndex), &ace))
		require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		sidCopy, sidCopyErr := aceSID.Copy()
		require.NoError(t, sidCopyErr)
		entries = append(entries, restrictedFileAccessEntry{sid: sidCopy, mask: ace.Mask})
	}
	return entries
}

func requireElevatedWindowsTest(t *testing.T) {
	t.Helper()

	isElevated, elevationErr := osutil.IsAdmin()
	require.NoError(t, elevationErr)
	if !isElevated {
		t.Skip("test requires an elevated Windows process")
	}
}

func createLegacyRestrictedFile(
	t *testing.T,
	path string,
	perm os.FileMode,
	principals restrictedDirectoryPrincipals,
) {
	t.Helper()

	explicitEntries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: expectedRestrictedFileUserAccess(perm),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(principals.tokenUser),
			},
		},
		{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(principals.system),
			},
		},
		{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(principals.admins),
			},
		},
	}
	acl, aclErr := windows.ACLFromEntries(explicitEntries, nil)
	require.NoError(t, aclErr)
	securityDescriptor, descriptorErr := windows.NewSecurityDescriptor()
	require.NoError(t, descriptorErr)
	require.NoError(t, securityDescriptor.SetDACL(acl, true, false))
	require.NoError(t, securityDescriptor.SetControl(
		windows.SE_DACL_PROTECTED,
		windows.SE_DACL_PROTECTED,
	))

	securityAttributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: securityDescriptor,
	}
	pathPointer, pathErr := windows.UTF16PtrFromString(path)
	require.NoError(t, pathErr)
	handle, createErr := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		securityAttributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	runtime.KeepAlive(securityDescriptor)
	runtime.KeepAlive(securityAttributes)
	require.NoError(t, createErr)
	require.NoError(t, windows.CloseHandle(handle))
}

func unusedDriveLetter(t *testing.T) string {
	t.Helper()

	drives, drivesErr := windows.GetLogicalDrives()
	require.NoError(t, drivesErr)
	for driveIndex := uint32(25); driveIndex >= 3; driveIndex-- {
		if drives&(1<<driveIndex) == 0 {
			return string(rune('A'+driveIndex)) + ":"
		}
	}
	t.Fatal("no unused drive letter is available")
	return ""
}
