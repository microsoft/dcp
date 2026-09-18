/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPollContainerResourceRemovedReturnsFalseWhenContextIsCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inspectCalled := false
	removed := pollContainerResourceRemoved(
		ctx,
		time.Hour,
		func(context.Context) (bool, error) {
			inspectCalled = true
			return false, nil
		},
		"Unexpected inspection failure",
		testutil.NewLogForTesting(t.Name()),
	)

	require.False(t, removed)
	require.False(t, inspectCalled)
}
