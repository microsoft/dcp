/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that bridge-owned progress survives controller and stale session writes, remains
// separate from sibling sessions, and cannot reopen a completed bridge lifetime.
func TestProjectDebugSessionProgressOwnership(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "progress-ownership"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"},
		}}}
	require.NoError(t, apiClient.Create(ctx, project))
	session := &apiv2.ProjectDebugSessionStatus{ID: "first", Phase: apiv2.ProjectDebugPhaseRunning}
	_, registerErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, registerErr)
	sibling := &apiv2.ProjectDebugSessionStatus{ID: "second", Phase: apiv2.ProjectDebugPhaseRunning}
	_, siblingErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, sibling)
	require.NoError(t, siblingErr)
	progress := &apiv2.ProjectDebugSessionProgress{Phase: apiv2.ProjectDebugConnectionConnected,
		StreamsConnected: true, InitializeRequested: true, AdapterInitialized: true, LaunchRequested: true}
	_, progressErr := writer.updateProgress(ctx, project.NamespacedName(), project.UID, session.ID, progress)
	require.NoError(t, progressErr)
	stale, staleErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, staleErr)
	require.Equal(t, progress, stale.Status.Debug.Sessions[0].Progress)
	requireReadyCondition := func(resource *apiv2.Project, ready metav1.ConditionStatus, reason string) {
		t.Helper()
		require.Equal(t, ready, resource.Status.Debug.Sessions[0].Conditions[0].Status)
		require.Equal(t, reason, resource.Status.Debug.Sessions[0].Conditions[0].Reason)
	}
	requireReadyCondition(stale, metav1.ConditionTrue, "DAPInitialized")
	require.Nil(t, stale.Status.Debug.Sessions[1].Progress)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Status.Debug.Sessions[0].Progress = nil
	project.Status.Phase = apiv2.ProjectPhaseReady
	require.NoError(t, apiClient.Status().Update(ctx, project))
	require.Equal(t, progress, project.Status.Debug.Sessions[0].Progress)
	progress.Phase, progress.ClientTerminating = apiv2.ProjectDebugConnectionTerminating, true
	terminating, terminationErr := writer.updateProgress(ctx, project.NamespacedName(), project.UID, session.ID, progress)
	require.NoError(t, terminationErr)
	requireReadyCondition(terminating, metav1.ConditionFalse, "SessionTerminating")
	progress.Phase = apiv2.ProjectDebugConnectionDisconnected
	progress.ClientDisconnected, progress.AdapterDisconnected = true, true
	_, disconnectErr := writer.updateProgress(ctx, project.NamespacedName(), project.UID, session.ID, progress)
	require.NoError(t, disconnectErr)
	session.Phase, session.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
	session.Conditions = debugConditions("SessionClosed", "Session closed.", false, project.Generation)
	completed, completionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, completionErr)
	require.Equal(t, progress, completed.Status.Debug.Sessions[0].Progress)
	session.Phase, session.FinishedAt = apiv2.ProjectDebugPhaseRunning, metav1.MicroTime{}
	stillCompleted, oldWriteErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, oldWriteErr)
	require.Equal(t, apiv2.ProjectDebugPhaseExited, stillCompleted.Status.Debug.Sessions[0].Phase)
	require.WithinDuration(t, completed.Status.Debug.Sessions[0].FinishedAt.Time,
		stillCompleted.Status.Debug.Sessions[0].FinishedAt.Time, time.Microsecond)
	require.Equal(t, progress, stillCompleted.Status.Debug.Sessions[0].Progress)
	_, wrongUIDErr := writer.updateProgress(ctx, project.NamespacedName(), "wrong-uid", session.ID, progress)
	require.True(t, apierrors.IsConflict(wrongUIDErr))
}
