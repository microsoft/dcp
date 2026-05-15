/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package bootstrap

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/notifications"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestShutdownHostServicesWaitsForGracefulControllerExit(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	shutdownErrors := make(chan error, 1)
	releaseHost := make(chan struct{})
	disposed := make(chan struct{})
	var hostCanceled atomic.Bool

	notificationSource := notifications.NotifySubscribersFunc(func(n notifications.Notification) error {
		require.Equal(t, notifications.NotificationKindShutdownRequested, n.Kind())
		return nil
	})

	go func() {
		<-releaseHost
		shutdownErrors <- nil
	}()

	result := make(chan error, 1)
	go func() {
		result <- shutdownHostServicesAndDisposeApiServer(
			notificationSource,
			shutdownErrors,
			func() { hostCanceled.Store(true) },
			func() error {
				close(disposed)
				return nil
			},
			time.Second,
			testutil.NewLogForTesting(t.Name()),
		)
	}()

	select {
	case <-disposed:
		t.Fatal("API server was disposed before the controller host exited")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseHost)

	select {
	case shutdownErr := <-result:
		require.NoError(t, shutdownErr)
	case <-ctx.Done():
		t.Fatal("Timed out waiting for shutdown to complete")
	}

	require.False(t, hostCanceled.Load(), "host context should not be canceled after graceful controller exit")
}

func TestShutdownHostServicesCancelsHostAfterControllerShutdownTimeout(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	shutdownErrors := make(chan error, 1)
	disposed := make(chan struct{})
	var hostCanceled atomic.Bool

	notificationSource := notifications.NotifySubscribersFunc(func(n notifications.Notification) error {
		require.Equal(t, notifications.NotificationKindShutdownRequested, n.Kind())
		return nil
	})

	shutdownErr := shutdownHostServicesAndDisposeApiServer(
		notificationSource,
		shutdownErrors,
		func() {
			hostCanceled.Store(true)
			shutdownErrors <- nil
		},
		func() error {
			close(disposed)
			return nil
		},
		10*time.Millisecond,
		testutil.NewLogForTesting(t.Name()),
	)

	require.Error(t, shutdownErr)
	require.Contains(t, shutdownErr.Error(), "controller host did not shut down")
	require.True(t, hostCanceled.Load(), "host context should be canceled after controller shutdown timeout")

	select {
	case <-disposed:
	case <-ctx.Done():
		t.Fatal("Timed out waiting for API server disposal")
	}
}

func TestShutdownHostServicesDisposesApiServerWhenHostShutdownFails(t *testing.T) {
	expectedErr := errors.New("host shutdown failed")
	shutdownErrors := make(chan error, 1)
	shutdownErrors <- expectedErr
	disposed := make(chan struct{})

	shutdownErr := shutdownHostServicesAndDisposeApiServer(
		nil,
		shutdownErrors,
		func() {},
		func() error {
			close(disposed)
			return nil
		},
		time.Second,
		testutil.NewLogForTesting(t.Name()),
	)

	require.ErrorIs(t, shutdownErr, expectedErr)
	select {
	case <-disposed:
	default:
		t.Fatal("API server was not disposed")
	}
}
