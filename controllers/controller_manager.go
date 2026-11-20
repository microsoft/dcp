/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"

	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"
)

// StartControllerManager runs a manager created with NewControllerManagerOptions until its lifetime context is cancelled.
// Cancellation is deferred until the manager has completed its initial cache startup.
func StartControllerManager(lifetimeCtx context.Context, mgr ctrl_manager.Manager) (managerRunErr error) {
	if initialContextErr := lifetimeCtx.Err(); initialContextErr != nil {
		if errors.Is(initialContextErr, context.Canceled) {
			return nil
		}
		return initialContextErr
	}

	managerCtx, cancelManager := context.WithCancelCause(context.WithoutCancel(lifetimeCtx))
	forwardingDone := make(chan struct{})
	var startupContextErr error

	go func() {
		defer close(forwardingDone)

		// controller-runtime can spin indefinitely if cancelled before cache readiness:
		// https://github.com/kubernetes-sigs/controller-runtime/issues/3629.
		// With LeaderElection disabled, Elected closes only after the manager consumes
		// its cache and initial runnable readiness notifications. A separate WaitForCacheSync
		// can return before that handoff, so it is not a safe cancellation barrier.
		// Remove this workaround once DCP adopts a controller-runtime release with the fix.
		select {
		case <-mgr.Elected():
			startupContextErr = lifetimeCtx.Err()
		case <-managerCtx.Done():
			return
		}

		select {
		case <-lifetimeCtx.Done():
			cancelManager(context.Cause(lifetimeCtx))
		case <-managerCtx.Done():
		}
	}()

	defer func() {
		cancelManager(nil)
		<-forwardingDone
		if managerRunErr == nil && errors.Is(startupContextErr, context.DeadlineExceeded) {
			managerRunErr = startupContextErr
		}
	}()

	return mgr.Start(managerCtx)
}
