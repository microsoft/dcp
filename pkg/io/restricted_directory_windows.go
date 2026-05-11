//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

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
	if !windows.EqualSid(owner, tokenUser.User.Sid) {
		return fmt.Errorf("directory owner does not match current user")
	}

	return nil
}
