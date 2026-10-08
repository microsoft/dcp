/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that root-exit confirmation skips waiting when a fatal stop failure
// did not produce a wait channel, without adding a cancellation error.
func TestWaitForProcessStopConfirmationSkipsMissingWaitChannel(t *testing.T) {
	t.Parallel()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	fatalStopErr := errors.New("root stop failed")
	waitErr := waitForProcessStopConfirmation(cancelledCtx, nil, fatalStopErr, processStopTimeout)

	require.NoError(t, waitErr)
}
