//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"os"
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
	acl, aclErr := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(worldSID),
			},
		},
	}, nil)
	require.NoError(t, aclErr)
	require.NoError(t, windows.SetNamedSecurityInfo(
		outputDir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))
	t.Cleanup(func() {
		_ = os.Chmod(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)
	})

	validateErr := usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.ErrorContains(t, validateErr, "disallowed principal")
}

func TestValidateRestrictedDirectoryRejectsDenyAccessEntry(t *testing.T) {
	outputDir := t.TempDir()
	worldSID, worldSIDErr := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, worldSIDErr)
	acl, aclErr := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(worldSID),
			},
		},
	}, nil)
	require.NoError(t, aclErr)
	require.NoError(t, windows.SetNamedSecurityInfo(
		outputDir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))
	t.Cleanup(func() {
		_ = os.Chmod(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)
	})

	validateErr := usvc_io.ValidateRestrictedDirectory(outputDir, osutil.PermissionOnlyOwnerReadWriteTraverse)

	require.ErrorContains(t, validateErr, "deny access entry")
}
