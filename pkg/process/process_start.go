/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
)

func rollbackProcessStart(ctx context.Context, kill func() error, wait func() error) (returnErr error) {
	defer func() { returnErr = uncertainProcessStart(returnErr) }()
	cleanupCtx, cleanupCancel := WithStopTimeout(ctx)
	defer cleanupCancel()
	killErr := kill()
	if IsProcessGoneErr(killErr) {
		killErr = nil
	}
	waitResult := make(chan error, 1)
	go func() {
		waitErr := wait()
		if IsEarlyProcessExitError(waitErr) {
			waitErr = nil
		}
		waitResult <- waitErr
	}()
	select {
	case <-cleanupCtx.Done():
		return errors.Join(killErr, cleanupCtx.Err())
	case waitErr, received := <-waitResult:
		if !received {
			return errors.Join(killErr, fmt.Errorf("rollback wait channel closed without a result"))
		}
		if waitErr == nil {
			return nil
		}
		return errors.Join(killErr, waitErr)
	}
}

func abortStartedProcess(waitable Waitable) error {
	cleanupCtx, cleanupCancel := WithStopTimeout(context.Background())
	defer cleanupCancel()
	return uncertainProcessStart(waitable.Abort(cleanupCtx))
}

func uncertainProcessStart(err error) error {
	if err == nil || errors.Is(err, ErrProcessStartUncertain) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrProcessStartUncertain, err)
}
