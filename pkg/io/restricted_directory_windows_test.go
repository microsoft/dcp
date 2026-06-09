//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestEnsureRestrictedDirectoryAppliesProtectedDACL(t *testing.T) {
	outputDir := t.TempDir()

	require.NoError(t, usvc_io.EnsureRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))

	require.NoError(t, usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
}

func TestValidateRestrictedDirectoryRejectsWorldAccess(t *testing.T) {
	outputDir := t.TempDir()
	worldSID, worldSIDErr := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, worldSIDErr)
	setDirectoryDACL(t, outputDir, true, []windows.EXPLICIT_ACCESS{
		directoryAccessEntry(t, currentUserSID(t), windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, 0),
		directoryAccessEntry(t, worldSID, windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	})

	validateErr := usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.ErrorContains(t, validateErr, "write or control access to disallowed principal")
}

func TestValidateRestrictedDirectoryAllowsInheritedReadOnlyAccess(t *testing.T) {
	worldSID, worldSIDErr := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, worldSIDErr)
	outputDir := directoryWithInheritedAccess(t, worldSID, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_EXECUTE)

	require.NoError(t, usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
}

func TestValidateRestrictedDirectoryRejectsInheritedWorldWriteAccess(t *testing.T) {
	worldSID, worldSIDErr := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, worldSIDErr)
	outputDir := directoryWithInheritedAccess(t, worldSID, windows.FILE_GENERIC_WRITE)

	validateErr := usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.ErrorContains(t, validateErr, "write or control access to disallowed principal")
}

func TestValidateRestrictedDirectoryAllowsDenyAccessEntry(t *testing.T) {
	outputDir := t.TempDir()
	worldSID, worldSIDErr := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, worldSIDErr)
	setDirectoryDACL(t, outputDir, true, []windows.EXPLICIT_ACCESS{
		directoryAccessEntry(t, currentUserSID(t), windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, 0),
		directoryAccessEntry(t, worldSID, windows.FILE_WRITE_DATA, windows.DENY_ACCESS, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	})

	require.NoError(t, usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
}

func TestValidateRestrictedDirectoryAllowsCreatorOwnerAccessEntry(t *testing.T) {
	outputDir := t.TempDir()
	creatorOwnerSID, creatorOwnerSIDErr := windows.CreateWellKnownSid(windows.WinCreatorOwnerSid)
	require.NoError(t, creatorOwnerSIDErr)
	setDirectoryDACL(t, outputDir, true, []windows.EXPLICIT_ACCESS{
		directoryAccessEntry(t, currentUserSID(t), windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, 0),
		directoryAccessEntry(t, creatorOwnerSID, windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	})

	require.NoError(t, usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
}

func currentUserSID(t *testing.T) *windows.SID {
	t.Helper()
	var processToken windows.Token
	tokenErr := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken)
	require.NoError(t, tokenErr)
	defer processToken.Close()

	tokenUser, tokenUserErr := processToken.GetTokenUser()
	require.NoError(t, tokenUserErr)
	userSID, copyErr := tokenUser.User.Sid.Copy()
	require.NoError(t, copyErr)
	return userSID
}

func directoryAccessEntry(t *testing.T, sid *windows.SID, accessPermissions windows.ACCESS_MASK, accessMode windows.ACCESS_MODE, inheritance uint32) windows.EXPLICIT_ACCESS {
	t.Helper()
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: accessPermissions,
		AccessMode:        accessMode,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func setDirectoryDACL(t *testing.T, outputDir string, protected bool, entries []windows.EXPLICIT_ACCESS) {
	t.Helper()
	acl, aclErr := windows.ACLFromEntries(entries, nil)
	require.NoError(t, aclErr)
	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		securityInfo |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	require.NoError(t, windows.SetNamedSecurityInfo(
		outputDir,
		windows.SE_FILE_OBJECT,
		securityInfo,
		nil,
		nil,
		acl,
		nil,
	))
	t.Cleanup(func() {
		_ = os.Chmod(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)
	})
}

func directoryWithInheritedAccess(t *testing.T, sid *windows.SID, accessPermissions windows.ACCESS_MASK) string {
	t.Helper()
	parentDir := t.TempDir()
	setDirectoryDACL(t, parentDir, false, []windows.EXPLICIT_ACCESS{
		directoryAccessEntry(t, currentUserSID(t), windows.STANDARD_RIGHTS_ALL|windows.GENERIC_ALL, windows.GRANT_ACCESS, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		directoryAccessEntry(t, sid, accessPermissions, windows.GRANT_ACCESS, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	})

	outputDir := filepath.Join(parentDir, "child")
	require.NoError(t, os.Mkdir(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
	return outputDir
}
