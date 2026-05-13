//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TOKEN_OWNER is the native TokenOwner result layout: a single PSID Owner
// field. x/sys/windows exposes the TokenOwner info class, but not the
// corresponding struct or a GetTokenOwner helper.
type tokenOwner struct {
	Owner *windows.SID
}

func validateRestrictedDirectoryOwner(dir string, _ os.FileInfo) error {
	var processToken windows.Token
	if tokenErr := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); tokenErr != nil {
		return fmt.Errorf("could not open process token: %w", tokenErr)
	}
	defer processToken.Close()

	tokenUser, tokenUserErr := processToken.GetTokenUser()
	if tokenUserErr != nil {
		return fmt.Errorf("could not get process token user: %w", tokenUserErr)
	}

	securityDescriptor, securityDescriptorErr := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if securityDescriptorErr != nil {
		return fmt.Errorf("could not get directory security descriptor: %w", securityDescriptorErr)
	}
	if securityDescriptor == nil {
		return fmt.Errorf("directory security descriptor is missing")
	}
	owner, _, ownerErr := securityDescriptor.Owner()
	if ownerErr != nil {
		return fmt.Errorf("could not get directory owner: %w", ownerErr)
	}
	// Elevated Windows tokens can create objects owned by the token owner SID
	// (for example, Administrators) rather than the token user SID. Accept both
	// so that a secure directory created by the current token is not rejected.
	ownerMatchesTokenOwner, tokenOwnerErr := tokenOwnerMatches(processToken, owner)
	if tokenOwnerErr != nil {
		return fmt.Errorf("could not compare process token owner: %w", tokenOwnerErr)
	}
	if !windows.EqualSid(owner, tokenUser.User.Sid) && !ownerMatchesTokenOwner {
		return fmt.Errorf("directory owner does not match current user or token owner")
	}

	return nil
}

func validateRestrictedDirectoryMode(os.FileInfo, os.FileMode) error {
	return nil
}

func tokenOwnerMatches(token windows.Token, owner *windows.SID) (bool, error) {
	var requiredLength uint32
	tokenOwnerErr := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &requiredLength)
	if tokenOwnerErr != windows.ERROR_INSUFFICIENT_BUFFER {
		return false, tokenOwnerErr
	}

	buffer := make([]byte, requiredLength)
	tokenOwnerErr = windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], uint32(len(buffer)), &requiredLength)
	if tokenOwnerErr != nil {
		return false, tokenOwnerErr
	}

	// GetTokenInformation(TokenOwner) fills the buffer with a native
	// TOKEN_OWNER structure. Cast the start of the buffer to that layout so we
	// can read the returned Owner SID.
	tokenOwnerInfo := (*tokenOwner)(unsafe.Pointer(&buffer[0]))
	return windows.EqualSid(owner, tokenOwnerInfo.Owner), nil
}
