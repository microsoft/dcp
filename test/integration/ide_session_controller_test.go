/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/ide"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const validIdeSessionLaunchConfigurations = `[{"type":"project","project_path":"./Foo.csproj","mode":"Debug"}]`

func makeIdeSession(name string, desiredState apiv1.IdeSessionState) *apiv1.IdeSession {
	return &apiv1.IdeSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.IdeSessionSpec{
			LaunchConfigurations: validIdeSessionLaunchConfigurations,
			DesiredState:         desiredState,
		},
	}
}

func waitForIdeSessionState(t *testing.T, ctx context.Context, name types.NamespacedName, target apiv1.IdeSessionState) *apiv1.IdeSession {
	t.Helper()
	return waitObjectAssumesState[apiv1.IdeSession](t, ctx, name, func(s *apiv1.IdeSession) (bool, error) {
		return s.Status.State == target, nil
	})
}

// waitForIdeSessionRunningWithSession waits for the IdeSession to reach Running and have a SessionID,
// then returns the matching TestIdeSession recorded by the test IDE client.
func waitForIdeSessionRunningWithSession(t *testing.T, ctx context.Context, name types.NamespacedName) (*apiv1.IdeSession, *ctrl_testutil.TestIdeSession) {
	t.Helper()
	running := waitObjectAssumesState[apiv1.IdeSession](t, ctx, name, func(s *apiv1.IdeSession) (bool, error) {
		return s.Status.State == apiv1.IdeSessionStateRunning && s.Status.SessionID != "", nil
	})

	sess, found := testIdeSessionClient.FindSession(ide.SessionID(running.Status.SessionID))
	require.True(t, found, "test IDE client should have recorded session '%s'", running.Status.SessionID)
	return running, sess
}

// TestIdeSessionStaysInitialWithoutDesiredRunning verifies that an IdeSession with the default
// (empty) DesiredState does not start, and that the controller does not contact the IDE for it.
func TestIdeSessionStaysInitialWithoutDesiredRunning(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-stays-initial-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateInitial)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	// Give the controller a chance to act (it should not).
	time.Sleep(2 * waitPollInterval)

	var observed apiv1.IdeSession
	require.NoError(t, client.Get(ctx, s.NamespacedName(), &observed))
	require.Equal(t, apiv1.IdeSessionStateInitial, observed.Status.State,
		"IdeSession should remain in the initial state until DesiredState=Running")
	require.Empty(t, observed.Status.SessionID,
		"controller should not have asked the IDE to start a session for this IdeSession")
}

// TestIdeSessionStartsThenStops drives an IdeSession through the full lifecycle:
// Initial → Starting → Running → Stopping → Stopped.
func TestIdeSessionStartsThenStops(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-lifecycle-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	_, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())

	// Simulate the IDE reporting a PID for the main process.
	require.NoError(t, testIdeSessionClient.SimulateProcessStart(sess.SessionID, process.Pid_t(12345)))

	withPid := waitObjectAssumesState[apiv1.IdeSession](t, ctx, s.NamespacedName(), func(o *apiv1.IdeSession) (bool, error) {
		return o.Status.PID != nil && *o.Status.PID == 12345, nil
	})
	require.False(t, withPid.Status.StartupTimestamp.IsZero(), "Status.StartupTimestamp should be set")

	// Ask the controller to stop the session.
	require.NoError(t, retryOnConflict[apiv1.IdeSession](ctx, s.NamespacedName(), func(_ context.Context, latest *apiv1.IdeSession) error {
		latest.Spec.DesiredState = apiv1.IdeSessionStateStopped
		return client.Update(ctx, latest)
	}))

	stopped := waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateStopped)
	require.False(t, stopped.Status.FinishTimestamp.IsZero(), "Status.FinishTimestamp should be set")
	require.Nil(t, stopped.Status.PID, "Status.PID should be cleared in terminal state")
}

// TestIdeSessionFailedStart verifies that if the IDE client refuses to start the session,
// the IdeSession transitions to Failed and the error message is reflected in Status.
func TestIdeSessionFailedStart(t *testing.T) {
	// NOTE: This test does not use t.Parallel() because it sets a one-shot startup-failure
	// flag on the shared test IDE client; running in parallel could allow another test's
	// StartSession call to consume the failure instead.

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	testIdeSessionClient.SetStartupFailure(errors.New("IDE rejected the launch configuration"))

	s := makeIdeSession(fmt.Sprintf("ides-failed-start-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	failed := waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateFailed)
	require.True(t,
		strings.Contains(failed.Status.Message, "IDE rejected the launch configuration"),
		"Status.Message should describe the IDE startup failure; got %q", failed.Status.Message,
	)
}

// TestIdeSessionTerminatesOnIdeNotification verifies that an OnTerminated notification
// from the IDE (with a non-zero exit code) lands the session in Failed.
func TestIdeSessionTerminatesOnIdeNotification(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-ide-term-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	_, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())

	nonZero := int32(7)
	require.NoError(t, testIdeSessionClient.SimulateRunEnd(sess.SessionID, &nonZero))

	failed := waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateFailed)
	require.NotNil(t, failed.Status.ExitCode, "Status.ExitCode should be set")
	require.Equal(t, int32(7), *failed.Status.ExitCode)
	require.False(t, failed.Status.FinishTimestamp.IsZero(), "Status.FinishTimestamp should be set on Failed")
}

// TestIdeSessionTerminatesCleanlyOnZeroExit verifies that a zero exit code from the IDE
// lands the session in Stopped (not Failed).
func TestIdeSessionTerminatesCleanlyOnZeroExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-zero-exit-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	_, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())

	zero := int32(0)
	require.NoError(t, testIdeSessionClient.SimulateRunEnd(sess.SessionID, &zero))

	stopped := waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateStopped)
	require.NotNil(t, stopped.Status.ExitCode)
	require.Equal(t, int32(0), *stopped.Status.ExitCode)
}

// TestIdeSessionDeletionStopsActiveSession verifies that deleting a Running IdeSession
// triggers a stop request to the IDE before the object is removed.
func TestIdeSessionDeletionStopsActiveSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-deletion-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))

	_, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())

	require.NoError(t, client.Delete(ctx, s))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, s)

	finalSess, _ := testIdeSessionClient.FindSession(sess.SessionID)
	if finalSess != nil {
		require.True(t, finalSess.Stopped, "session should have been stopped before deletion completed")
	}
}

// TestIdeSessionRejectsLaunchConfigChange verifies that the API server rejects an update
// that changes Spec.LaunchConfigurations.
func TestIdeSessionRejectsLaunchConfigChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-immutable-lc-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateInitial)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	updateErr := retryOnConflict[apiv1.IdeSession](ctx, s.NamespacedName(), func(_ context.Context, latest *apiv1.IdeSession) error {
		latest.Spec.LaunchConfigurations = `[{"type":"project","project_path":"./Bar.csproj"}]`
		return client.Update(ctx, latest)
	})
	require.Error(t, updateErr, "API server should reject changes to LaunchConfigurations")
}

// TestIdeSessionForwardsLaunchConfigurationsToIde verifies that the LaunchConfigurations on the
// spec are forwarded to the IDE unchanged.
func TestIdeSessionForwardsLaunchConfigurationsToIde(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-lc-fwd-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	_, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())
	require.JSONEq(t, validIdeSessionLaunchConfigurations, string(sess.Request.LaunchConfigurations),
		"LaunchConfigurations forwarded to the IDE should match the spec")

	// Avoid leaking the session beyond the test.
	zero := int32(0)
	require.NoError(t, testIdeSessionClient.SimulateRunEnd(sess.SessionID, &zero))
	_ = waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateStopped)
}

// TestIdeSessionRecordsStdOutFile verifies that Status.StdOutFile is populated once the session
// is running, which is a precondition for log retrieval via the logs sub-resource.
func TestIdeSessionRecordsStdOutFile(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	s := makeIdeSession(fmt.Sprintf("ides-stdout-file-%s", strings.ToLower(t.Name())), apiv1.IdeSessionStateRunning)
	require.NoError(t, client.Create(ctx, s))
	defer func() {
		_ = client.Delete(context.Background(), s)
	}()

	running, sess := waitForIdeSessionRunningWithSession(t, ctx, s.NamespacedName())
	require.NotEmpty(t, running.Status.StdOutFile, "Status.StdOutFile should be populated once running")
	require.NotEmpty(t, running.Status.StdErrFile, "Status.StdErrFile should be populated once running")

	// Drive the session to a clean stop so resources are cleaned up promptly.
	zero := int32(0)
	require.NoError(t, testIdeSessionClient.SimulateRunEnd(sess.SessionID, &zero))
	_ = waitForIdeSessionState(t, ctx, s.NamespacedName(), apiv1.IdeSessionStateStopped)
}