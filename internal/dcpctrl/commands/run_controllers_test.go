/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/notifications"
)

// Verifies that handleNotification ends the controller lifetime when a shutdown is requested.
func TestHandleNotificationShutsDownOnShutdownRequest(t *testing.T) {
	t.Parallel()

	ctrlCtx, ctrlCtxCancel := context.WithCancel(context.Background())
	defer ctrlCtxCancel()

	handleNotification(ctrlCtx, ctrlCtxCancel, &notifications.ShutdownRequestedNotification{}, logr.Discard())
	require.ErrorIs(t, ctrlCtx.Err(), context.Canceled)
}
