/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"fmt"

	"github.com/microsoft/dcp/pkg/process"
)

func CurrentResourceLeaseOwner() (process.ProcessHandle, error) {
	currentProcess, currentProcessErr := process.This()
	if currentProcessErr != nil {
		return process.ProcessHandle{}, fmt.Errorf("could not get current process identity: %w", currentProcessErr)
	}

	return normalizeResourceLeaseOwner(currentProcess)
}

func normalizeResourceLeaseOwner(owner process.ProcessHandle) (process.ProcessHandle, error) {
	if owner.Pid <= 0 {
		return process.ProcessHandle{}, fmt.Errorf("resource lease owner process ID must be positive")
	}
	if owner.IdentityTime.IsZero() {
		return process.ProcessHandle{}, fmt.Errorf("resource lease owner process identity time cannot be zero")
	}

	return process.ProcessHandle{
		Pid:          owner.Pid,
		IdentityTime: owner.IdentityTime.UTC(),
	}, nil
}

func resourceLeaseOwnerFromDB(ownerPID int64, ownerIdentityTime string) (process.ProcessHandle, error) {
	pid, pidConvertErr := process.Int64_ToPidT(ownerPID)
	if pidConvertErr != nil {
		return process.ProcessHandle{}, fmt.Errorf("could not convert resource lease owner process ID: %w", pidConvertErr)
	}
	identityTime, identityTimeErr := timeFromString(ownerIdentityTime)
	if identityTimeErr != nil {
		return process.ProcessHandle{}, fmt.Errorf("could not parse resource lease owner process identity time: %w", identityTimeErr)
	}

	owner := process.ProcessHandle{
		Pid:          pid,
		IdentityTime: identityTime,
	}
	return normalizeResourceLeaseOwner(owner)
}

func resourceLeaseOwnerIsActive(owner process.ProcessHandle) bool {
	normalizedOwner, normalizeErr := normalizeResourceLeaseOwner(owner)
	if normalizeErr != nil {
		return false
	}

	_, findErr := process.FindProcess(normalizedOwner)
	return findErr == nil
}
