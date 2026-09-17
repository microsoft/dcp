/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/resiliency"
)

var errContainerResourceNotRemoved = errors.New("container resource has not been removed")

func pollContainerResourceRemoved(
	ctx context.Context,
	pollInterval time.Duration,
	inspect func(context.Context) (bool, error),
	inspectFailureMessage string,
	log logr.Logger,
) bool {
	if ctx.Err() != nil {
		return false
	}

	pollBackoff := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(pollInterval),
		backoff.WithMaxInterval(pollInterval),
		backoff.WithMaxElapsedTime(0),
		backoff.WithRandomizationFactor(0.05),
		backoff.WithMultiplier(1),
	)
	pollErr := resiliency.Retry(ctx, pollBackoff, func() error {
		removed, inspectErr := inspect(ctx)
		if removed {
			return nil
		}
		if inspectErr != nil {
			if ctx.Err() != nil {
				return inspectErr
			}
			log.Error(inspectErr, inspectFailureMessage)
			return inspectErr
		}
		return errContainerResourceNotRemoved
	})
	return pollErr == nil
}
