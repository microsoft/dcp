/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/statestore"
)

func logResourceLeaseHeld(log logr.Logger, leaseErr error, resourceKey string, message string) {
	logValues := []any{"ResourceKey", resourceKey}
	if heldLease, found := statestore.HeldResourceLease(leaseErr); found {
		logValues = []any{
			"ResourceKey", heldLease.ResourceKey,
			"LeaseOwnerPID", heldLease.OwnerProcess.Pid,
			"LeaseOwnerIdentityTime", heldLease.OwnerProcess.IdentityTime,
		}
	}

	log.V(1).Info(message, logValues...)
}

func logResourceLeaseNotHeld(log logr.Logger, suppress bool, resourceKey string, message string) {
	if suppress {
		return
	}
	log.V(1).Info(message, "ResourceKey", resourceKey)
}
