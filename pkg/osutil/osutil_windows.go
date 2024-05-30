//go:build windows

package osutil

import (
	"golang.org/x/sys/windows"
)

func IsAdmin() (bool, error) {
	// Get the actual token for the process
	var processToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); err != nil {
		return true, err
	}
	defer processToken.Close()

	// Get the SID for the Administrators group
	adminSid, err := GetBuiltInSid(windows.DOMAIN_ALIAS_RID_ADMINS)
	if err != nil {
		return true, err
	}

	// nolint:errcheck
	defer windows.FreeSid(adminSid)

	// Get a virtual token for the process (not the actual token) to determine if the user is an admin
	adminToken := windows.Token(0)
	isAdmin, err := adminToken.IsMember(adminSid)
	if err != nil {
		return true, err
	}

	return isAdmin, nil
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
