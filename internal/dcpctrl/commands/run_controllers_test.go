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

	"github.com/microsoft/dcp/internal/notifications"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestShutdownRequestedNotificationCancelsControllerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handleNotification(
		ctx,
		cancel,
		&notifications.ShutdownRequestedNotification{},
		testutil.NewLogForTesting(t.Name()),
	)

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for controller context cancellation")
	}

	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
