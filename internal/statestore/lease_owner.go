/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"fmt"

	"github.com/microsoft/dcp/pkg/process"
)

func CurrentResourceLeaseOwner() (process.ProcessTreeItem, error) {
	currentProcess, currentProcessErr := process.This()
	if currentProcessErr != nil {
		return process.ProcessTreeItem{}, fmt.Errorf("could not get current process identity: %w", currentProcessErr)
	}

	return normalizeResourceLeaseOwner(currentProcess)
}

func normalizeResourceLeaseOwner(owner process.ProcessTreeItem) (process.ProcessTreeItem, error) {
	if owner.Pid <= 0 {
		return process.ProcessTreeItem{}, fmt.Errorf("resource lease owner process ID must be positive")
	}
	if owner.IdentityTime.IsZero() {
		return process.ProcessTreeItem{}, fmt.Errorf("resource lease owner process identity time cannot be zero")
	}

	return process.ProcessTreeItem{
		Pid:          owner.Pid,
		IdentityTime: owner.IdentityTime.UTC(),
	}, nil
}

func resourceLeaseOwnerFromDB(ownerPID int64, ownerIdentityTimeUnixNano int64) (process.ProcessTreeItem, error) {
	pid, pidConvertErr := process.Int64_ToPidT(ownerPID)
	if pidConvertErr != nil {
		return process.ProcessTreeItem{}, fmt.Errorf("could not convert resource lease owner process ID: %w", pidConvertErr)
	}

	owner := process.ProcessTreeItem{
		Pid:          pid,
		IdentityTime: timeFromUnixNano(ownerIdentityTimeUnixNano),
	}
	return normalizeResourceLeaseOwner(owner)
}

func resourceLeaseOwnerIsActive(owner process.ProcessTreeItem) bool {
	normalizedOwner, normalizeErr := normalizeResourceLeaseOwner(owner)
	if normalizeErr != nil {
		return false
	}

	_, findErr := process.FindProcess(normalizedOwner.Pid, normalizedOwner.IdentityTime)
	return findErr == nil
}
