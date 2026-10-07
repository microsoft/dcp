/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"time"
)

// ProcessGroup identifies an isolated Unix process group by its original leader.
// Capture it while the leader is running to monitor and stop members that outlive the leader.
type ProcessGroup struct {
	WaitPollInterval time.Duration
	leader           ProcessHandle
}

// Wait waits until the group has no live members, or the context is canceled.
// Exited processes awaiting reaping do not keep the group alive.
func (g *ProcessGroup) Wait(ctx context.Context) error {
	return g.wait(ctx, g.WaitPollInterval)
}

func (g *ProcessGroup) wait(ctx context.Context, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		pollInterval = defaultWaitPollInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			running, runningErr := g.isRunning(ctx)
			if runningErr != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				return runningErr
			}
			if !running {
				return nil
			}
			timer.Reset(pollInterval)
		}
	}
}
