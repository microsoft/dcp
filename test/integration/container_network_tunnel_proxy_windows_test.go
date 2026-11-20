//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"path/filepath"
	std_slices "slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func requireTunnelProxyCredentialPermissions(t *testing.T, path string) {
	t.Helper()
	directory := filepath.Dir(path)
	require.NoError(t, usvc_io.ValidateRestrictedDirectory(directory, osutil.PermissionOnlyOwnerReadWriteTraverse))
	var processToken windows.Token
	require.NoError(t, windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken))
	defer processToken.Close()
	tokenUser, userErr := processToken.GetTokenUser()
	require.NoError(t, userErr)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(t, systemErr)
	adminsSID, adminsErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(t, adminsErr)
	directoryDescriptor, directoryErr := windows.GetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	require.NoError(t, directoryErr)
	directoryOwner, _, ownerErr := directoryDescriptor.Owner()
	require.NoError(t, ownerErr)
	allowedSIDs := []*windows.SID{tokenUser.User.Sid, directoryOwner, systemSID, adminsSID}
	for _, protectedPath := range []string{directory, path} {
		descriptor, descriptorErr := windows.GetNamedSecurityInfo(protectedPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(t, descriptorErr)
		dacl, _, daclErr := descriptor.DACL()
		require.NoError(t, daclErr)
		require.NotNil(t, dacl, "A null DACL would allow every user access")
		require.Positive(t, dacl.AceCount)
		for entryIndex := uint16(0); entryIndex < dacl.AceCount; entryIndex++ {
			var entry *windows.ACCESS_ALLOWED_ACE
			require.NoError(t, windows.GetAce(dacl, uint32(entryIndex), &entry))
			if entry.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
				continue
			}
			require.EqualValues(t, windows.ACCESS_ALLOWED_ACE_TYPE, entry.Header.AceType)
			entrySID := (*windows.SID)(unsafe.Pointer(&entry.SidStart))
			require.True(t, std_slices.ContainsFunc(allowedSIDs, func(allowed *windows.SID) bool {
				return windows.EqualSid(allowed, entrySID)
			}), "Credential access must not be granted to unrelated users")
		}
	}
}
