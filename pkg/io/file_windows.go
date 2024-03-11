//go:build windows

package io

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"golang.org/x/sys/windows"
)

// Open a file on Windows. If the process is running as an administrator, we want to ensure that
// the file is only readable by other elevated processes. If not running as administrator, we
// simply use the standard os.OpenFile function.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if flag == os.O_RDONLY {
		// If we are only reading the file, we don't need to do anything special
		return os.OpenFile(name, flag, perm)
	}

	// Get the actual token for the process
	var processToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); err != nil {
		return nil, err
	}
	defer processToken.Close()

	// Get the SID for the Administrators group
	adminSid, err := GetBuiltInSid(windows.DOMAIN_ALIAS_RID_ADMINS)
	if err != nil {
		return nil, err
	}
	defer func() {
		if freeSidErr := windows.FreeSid(adminSid); err != nil {
			fmt.Fprintln(os.Stderr, fmt.Errorf("could not free sid: %w", freeSidErr))
		}
	}()

	// Get a virtual token for the process (not the actual token) to determine if the user is an admin
	adminToken := windows.Token(0)
	isAdmin, err := adminToken.IsMember(adminSid)
	if err != nil {
		return nil, err
	}

	if !isAdmin {
		return os.OpenFile(name, flag, perm)
	}

	// Get the user who ran the process so we can get the SID
	tokenUser, err := processToken.GetTokenUser()
	if err != nil {
		return nil, err
	}

	// Get the SID for the Local System account
	systemSid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}

	var standardUserAccessPermissions windows.ACCESS_MASK = windows.READ_CONTROL | windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA

	if perm&osutil.PermissionGroupRead == osutil.PermissionGroupRead {
		standardUserAccessPermissions |= windows.FILE_GENERIC_READ
	}

	var explicitEntries []windows.EXPLICIT_ACCESS
	// Add an ACL entry for the user running the process
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the user permission to read the ACL list for the file, read attributes, and delete the file
			// DO NOT grant read permission as we want to limit access to the file to elevated processes only
			AccessPermissions: standardUserAccessPermissions,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(tokenUser.User.Sid),
			},
		},
	)

	// Add an ACL entry for the System account
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the System SID standard permissions to the file
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSid),
			},
		},
	)

	// And an ACL entry for the Administrators group
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the Administrators SID standard permissions to the file
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSid),
			},
		},
	)

	acl, err := windows.ACLFromEntries(explicitEntries, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create acl: %w", err)
	}

	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("could not create security descriptor: %w", err)
	}

	if err = sd.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("could not set dacl: %w", err)
	}

	// Ensure that the Security Descriptor applies the ACL and does not inherit permissions from the parent directory
	if err = sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("could not set control flag: %w", err)
	}

	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}

	pathHandle, pathErr := windows.UTF16PtrFromString(name)
	if pathErr != nil {
		return nil, fmt.Errorf("could not create path handle: %w", pathErr)
	}

	// Create the new file with the given ACL rules
	fileHandle, fileCreateErr := windows.CreateFile(pathHandle, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if fileCreateErr != nil {
		return nil, fmt.Errorf("could not create file: %w", fileCreateErr)
	}

	return os.NewFile(uintptr(fileHandle), name), nil
}

// Write to a file on Windows. If the process is running as an administrator, we want to ensure that
// the file is only readable by other elevated processes. If not running as an administrator, we
// simply use the standard os.WriteFile function.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	file, err := OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	_, err = file.Write(data)
	if err1 := file.Close(); err1 != nil || err != nil {
		return errors.Join(err, err1)
	}

	return nil
}

func GetBuiltInSid(domainAliasRid uint32) (*windows.SID, error) {
	var sid *windows.SID
	if err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		domainAliasRid,
		0, 0, 0, 0, 0, 0,
		&sid,
	); err != nil {
		return nil, err
	}

	return sid, nil
}
