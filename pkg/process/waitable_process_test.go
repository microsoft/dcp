/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitPollPolicySwitchesToSteadyInterval(t *testing.T) {
	t.Parallel()

	policy := waitPollPolicy{
		initialInterval: 100 * time.Millisecond,
		initialDuration: processStopTimeout,
		steadyInterval:  defaultWaitPollInterval,
	}

	require.Equal(t, 100*time.Millisecond, policy.intervalAfter(0))
	require.Equal(t, 100*time.Millisecond, policy.intervalAfter(processStopTimeout-time.Nanosecond))
	require.Equal(t, defaultWaitPollInterval, policy.intervalAfter(processStopTimeout))
	require.Equal(t, defaultWaitPollInterval, policy.intervalAfter(processStopTimeout+time.Hour))
}

func TestFixedWaitPollPolicyNeverChangesInterval(t *testing.T) {
	t.Parallel()

	policy := fixedWaitPollPolicy(defaultWaitPollInterval)

	require.Equal(t, defaultWaitPollInterval, policy.intervalAfter(0))
	require.Equal(t, defaultWaitPollInterval, policy.intervalAfter(24*time.Hour))
}
