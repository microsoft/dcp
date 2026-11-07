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

// Verifies that protocol observations preserve out-of-order outcomes and sibling sessions through
// stale lifetime/controller writes, and interrupt only requests awaiting responses at closure.
func TestProjectDebugProtocolFieldOwnership(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "protocol-ownership"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"},
		}}}
	require.NoError(t, apiClient.Create(ctx, project))
	session := &apiv2.ProjectDebugSessionStatus{ID: "first", Phase: apiv2.ProjectDebugPhaseRunning}
	_, sessionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, sessionErr)
	sibling := &apiv2.ProjectDebugSessionStatus{ID: "second", Phase: apiv2.ProjectDebugPhaseRunning}
	_, siblingErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, sibling)
	require.NoError(t, siblingErr)
	for index, command := range []string{"initialize", "launch", "attach"} {
		_, requestErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID,
			apiv2.ProjectDebugRequestStatus{RequestSeq: int64(index + 11), Command: command, Phase: apiv2.ProjectDebugRequestPending})
		require.NoError(t, requestErr)
	}
	_, siblingRequestErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, sibling.ID,
		apiv2.ProjectDebugRequestStatus{RequestSeq: 11, Command: "initialize", Phase: apiv2.ProjectDebugRequestPending})
	require.NoError(t, siblingRequestErr)
	rejected := apiv2.ProjectDebugRequestStatus{RequestSeq: 12, Command: "launch", Phase: apiv2.ProjectDebugRequestFailed,
		FinishedAt: metav1.NowMicro(), Message: "launch rejected"}
	_, rejectionErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID, rejected)
	require.NoError(t, rejectionErr)
	initialized := apiv2.ProjectDebugRequestStatus{RequestSeq: 11, Command: "initialize",
		Phase: apiv2.ProjectDebugRequestSucceeded, FinishedAt: metav1.NowMicro()}
	_, initializationErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID, initialized)
	require.NoError(t, initializationErr)
	_, duplicateErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID,
		apiv2.ProjectDebugRequestStatus{RequestSeq: 12, Command: "launch", Phase: apiv2.ProjectDebugRequestPending})
	require.NoError(t, duplicateErr)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Status.Debug.Sessions[0].ProtocolRequests = nil
	project.Status.Phase = apiv2.ProjectPhaseReady
	require.NoError(t, apiClient.Status().Update(ctx, project))
	require.Len(t, project.Status.Debug.Sessions[0].ProtocolRequests, 3)
	session.Phase, session.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
	completed, completionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, completionErr)
	observations := completed.Status.Debug.Sessions[0].ProtocolRequests
	require.Equal(t, apiv2.ProjectDebugRequestSucceeded, observations[0].Phase)
	require.Equal(t, apiv2.ProjectDebugRequestFailed, observations[1].Phase)
	require.Equal(t, "launch rejected", observations[1].Message)
	require.Equal(t, apiv2.ProjectDebugRequestInterrupted, observations[2].Phase)
	require.WithinDuration(t, session.FinishedAt.Time, observations[2].FinishedAt.Time, time.Microsecond)
	require.Equal(t, apiv2.ProjectDebugRequestPending, completed.Status.Debug.Sessions[1].ProtocolRequests[0].Phase)
	require.Equal(t, apiv2.ProjectPhaseReady, completed.Status.Phase)
	_, lateResponseErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID,
		apiv2.ProjectDebugRequestStatus{RequestSeq: 13, Command: "attach", Phase: apiv2.ProjectDebugRequestSucceeded, FinishedAt: metav1.NowMicro()})
	require.NoError(t, lateResponseErr)
	late, lateRequestErr := writer.observeRequest(ctx, project.NamespacedName(), project.UID, session.ID,
		apiv2.ProjectDebugRequestStatus{RequestSeq: 14, Command: "launch", Phase: apiv2.ProjectDebugRequestPending})
	require.NoError(t, lateRequestErr)
	require.Equal(t, apiv2.ProjectDebugRequestInterrupted, late.Status.Debug.Sessions[0].ProtocolRequests[2].Phase)
	require.Equal(t, apiv2.ProjectDebugRequestInterrupted, late.Status.Debug.Sessions[0].ProtocolRequests[3].Phase)
	_, wrongUIDErr := writer.observeRequest(ctx, project.NamespacedName(), "wrong-uid", session.ID, initialized)
	require.True(t, apierrors.IsConflict(wrongUIDErr))
}
