/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"math/rand"
	"time"

	"github.com/go-logr/logr"
)

func pollContainerResourceRemoved(
	ctx context.Context,
	pollInterval time.Duration,
	inspect func(context.Context) (bool, error),
	inspectFailureMessage string,
	log logr.Logger,
) bool {
	timer := time.NewTimer(containerResourcePollDelay(pollInterval))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			removed, inspectErr := inspect(ctx)
			if removed {
				return true
			}
			if inspectErr != nil {
				if ctx.Err() != nil {
					return false
				}
				log.Error(inspectErr, inspectFailureMessage)
			}
			timer.Reset(containerResourcePollDelay(pollInterval))
		}
	}
}

func containerResourcePollDelay(pollInterval time.Duration) time.Duration {
	jitterRange := pollInterval / 20
	if jitterRange <= 0 {
		return pollInterval
	}
	return pollInterval + time.Duration(rand.Int63n(int64(jitterRange)))
}
