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

	"github.com/microsoft/dcp/pkg/osutil"
	"golang.org/x/sys/windows"
)

// TOKEN_OWNER is the native TokenOwner result layout: a single PSID Owner
// field. x/sys/windows exposes the TokenOwner info class, but not the
// corresponding struct or a GetTokenOwner helper.
type tokenOwner struct {
	Owner *windows.SID
}

type restrictedDirectoryPrincipals struct {
	tokenUser  *windows.SID
	tokenOwner *windows.SID
	system     *windows.SID
	admins     *windows.SID
}

const (
	restrictedDirectoryAccessMask windows.ACCESS_MASK = windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL

	// FILE_DELETE_CHILD is directory-specific and is not exposed by x/sys/windows.
	restrictedDirectoryDeleteChildAccess windows.ACCESS_MASK = 0x00000040

	restrictedDirectoryDangerousAccessMask windows.ACCESS_MASK = windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES |
		restrictedDirectoryDeleteChildAccess |
		windows.DELETE |
		windows.WRITE_DAC |
		windows.WRITE_OWNER |
		windows.MAXIMUM_ALLOWED |
		windows.GENERIC_WRITE |
		windows.GENERIC_ALL
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

func restrictRestrictedDirectory(dir string, perm os.FileMode) error {
	if perm != osutil.PermissionOnlyOwnerReadWriteTraverse {
		return fmt.Errorf("unsupported restricted directory permissions %s", perm)
	}

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	if principalErr != nil {
		return principalErr
	}
	acl, aclErr := restrictedDirectoryACL(principals)
	if aclErr != nil {
		return aclErr
	}

	if setErr := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); setErr != nil {
		return fmt.Errorf("could not set restricted directory dacl: %w", setErr)
	}
	return nil
}

func validateRestrictedDirectoryMode(dir string, _ os.FileInfo, perm os.FileMode) error {
	if perm != osutil.PermissionOnlyOwnerReadWriteTraverse {
		return fmt.Errorf("unsupported restricted directory permissions %s", perm)
	}

	securityDescriptor, securityDescriptorErr := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if securityDescriptorErr != nil {
		return fmt.Errorf("could not get directory security descriptor: %w", securityDescriptorErr)
	}
	if securityDescriptor == nil {
		return fmt.Errorf("directory security descriptor is missing")
	}

	dacl, _, daclErr := securityDescriptor.DACL()
	if daclErr != nil {
		return fmt.Errorf("could not get directory dacl: %w", daclErr)
	}
	if dacl == nil {
		return fmt.Errorf("directory dacl is empty")
	}

	principals, principalErr := currentRestrictedDirectoryPrincipals()
	if principalErr != nil {
		return principalErr
	}
	allowedSIDs, allowedSIDErr := allowedRestrictedDirectoryValidationSIDs(principals)
	if allowedSIDErr != nil {
		return allowedSIDErr
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if aceErr := windows.GetAce(dacl, uint32(i), &ace); aceErr != nil {
			return fmt.Errorf("could not inspect directory dacl entry %d: %w", i, aceErr)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			aceSid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sidMatchesAny(aceSid, allowedSIDs) && aceHasDangerousAccess(ace.Mask) {
				return fmt.Errorf("directory dacl grants write or control access to disallowed principal %s", aceSid.String())
			}
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		default:
			return fmt.Errorf("directory dacl contains unsupported access entry type %d", ace.Header.AceType)
		}
	}

	return nil
}

func aceHasDangerousAccess(accessMask windows.ACCESS_MASK) bool {
	return accessMask&restrictedDirectoryDangerousAccessMask != 0
}

func restrictedDirectoryACL(principals restrictedDirectoryPrincipals) (*windows.ACL, error) {
	explicitEntries := make([]windows.EXPLICIT_ACCESS, 0, 4)
	for _, sid := range uniqueRestrictedDirectoryPrincipals(principals) {
		explicitEntries = append(explicitEntries, windows.EXPLICIT_ACCESS{
			AccessPermissions: restrictedDirectoryAccessMask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}

	acl, aclErr := windows.ACLFromEntries(explicitEntries, nil)
	if aclErr != nil {
		return nil, fmt.Errorf("could not create restricted directory dacl: %w", aclErr)
	}
	return acl, nil
}

func currentRestrictedDirectoryPrincipals() (restrictedDirectoryPrincipals, error) {
	var processToken windows.Token
	if tokenErr := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); tokenErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not open process token: %w", tokenErr)
	}
	defer processToken.Close()

	tokenUser, tokenUserErr := processToken.GetTokenUser()
	if tokenUserErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not get process token user: %w", tokenUserErr)
	}
	tokenUserSID, tokenUserCopyErr := tokenUser.User.Sid.Copy()
	if tokenUserCopyErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not copy process token user sid: %w", tokenUserCopyErr)
	}
	tokenOwnerPrincipal, tokenOwnerErr := tokenOwnerSID(processToken)
	if tokenOwnerErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not get process token owner: %w", tokenOwnerErr)
	}
	systemSID, systemSIDErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if systemSIDErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not get local system sid: %w", systemSIDErr)
	}
	adminSID, adminSIDErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if adminSIDErr != nil {
		return restrictedDirectoryPrincipals{}, fmt.Errorf("could not get administrators sid: %w", adminSIDErr)
	}

	return restrictedDirectoryPrincipals{
		tokenUser:  tokenUserSID,
		tokenOwner: tokenOwnerPrincipal,
		system:     systemSID,
		admins:     adminSID,
	}, nil
}

func uniqueRestrictedDirectoryPrincipals(principals restrictedDirectoryPrincipals) []*windows.SID {
	var uniqueSIDs []*windows.SID
	for _, sid := range []*windows.SID{principals.tokenUser, principals.tokenOwner, principals.system, principals.admins} {
		if sid != nil && !sidMatchesAny(sid, uniqueSIDs) {
			uniqueSIDs = append(uniqueSIDs, sid)
		}
	}
	return uniqueSIDs
}

func allowedRestrictedDirectoryValidationSIDs(principals restrictedDirectoryPrincipals) ([]*windows.SID, error) {
	allowedSIDs := uniqueRestrictedDirectoryPrincipals(principals)
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinCreatorOwnerSid,
		windows.WinCreatorOwnerRightsSid,
	} {
		sid, sidErr := windows.CreateWellKnownSid(sidType)
		if sidErr != nil {
			return nil, fmt.Errorf("could not get well-known sid %d: %w", sidType, sidErr)
		}
		if !sidMatchesAny(sid, allowedSIDs) {
			allowedSIDs = append(allowedSIDs, sid)
		}
	}
	return allowedSIDs, nil
}

func sidMatchesAny(sid *windows.SID, candidates []*windows.SID) bool {
	for _, candidate := range candidates {
		if windows.EqualSid(sid, candidate) {
			return true
		}
	}
	return false
}

func tokenOwnerMatches(token windows.Token, owner *windows.SID) (bool, error) {
	tokenOwnerPrincipal, tokenOwnerErr := tokenOwnerSID(token)
	if tokenOwnerErr != nil {
		return false, tokenOwnerErr
	}

	return windows.EqualSid(owner, tokenOwnerPrincipal), nil
}

func tokenOwnerSID(token windows.Token) (*windows.SID, error) {
	var requiredLength uint32
	tokenOwnerErr := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &requiredLength)
	if tokenOwnerErr != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, tokenOwnerErr
	}

	buffer := make([]byte, requiredLength)
	tokenOwnerErr = windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], uint32(len(buffer)), &requiredLength)
	if tokenOwnerErr != nil {
		return nil, tokenOwnerErr
	}

	// GetTokenInformation(TokenOwner) fills the buffer with a native
	// TOKEN_OWNER structure. Cast the start of the buffer to that layout so we
	// can read the returned Owner SID.
	tokenOwnerInfo := (*tokenOwner)(unsafe.Pointer(&buffer[0]))
	tokenOwnerCopy, tokenOwnerCopyErr := tokenOwnerInfo.Owner.Copy()
	if tokenOwnerCopyErr != nil {
		return nil, tokenOwnerCopyErr
	}
	return tokenOwnerCopy, nil
}
