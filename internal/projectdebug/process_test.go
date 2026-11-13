/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that process registration deduplicates captured identities across API precision loss, preserves PID reuse,
// isolates controller observations from session completion, and accepts late reports during stop.
func TestProjectDebugProcessFieldOwnership(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "process-ownership"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"},
		}}}
	require.NoError(t, apiClient.Create(ctx, project))
	session := &apiv2.ProjectDebugSessionStatus{ID: "session", Phase: apiv2.ProjectDebugPhaseRunning}
	_, sessionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, sessionErr)
	unconfirmed, unconfirmedErr := writer.registerProcess(ctx, project.NamespacedName(), project.UID, session.ID, nil)
	require.NoError(t, unconfirmedErr)
	require.True(t, unconfirmed.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	pid := int64(123)
	first := &apiv2.ProjectDebugProcessStatus{ID: "first", PID: &pid,
		IdentityTimestamp: metav1.NewMicroTime(time.Unix(1_700_000_000, 123_456_789)), Name: "target"}
	registered, registrationErr := writer.registerProcess(ctx, project.NamespacedName(), project.UID, session.ID, first)
	require.NoError(t, registrationErr)
	require.False(t, registered.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), registered))
	require.False(t, registered.Status.Debug.Sessions[0].Processes[0].IdentityTimestamp.Equal(&first.IdentityTimestamp))
	duplicate := first.DeepCopy()
	duplicate.ID = "duplicate"
	deduplicated, duplicateErr := writer.registerProcess(ctx, project.NamespacedName(), project.UID, session.ID, duplicate)
	require.NoError(t, duplicateErr)
	require.Len(t, deduplicated.Status.Debug.Sessions[0].Processes, 1)
	reused := first.DeepCopy()
	reused.ID = "reused"
	reused.IdentityTimestamp = metav1.NewMicroTime(first.IdentityTimestamp.Add(3 * time.Microsecond))
	registered, registrationErr = writer.registerProcess(ctx, project.NamespacedName(), project.UID, session.ID, reused)
	require.NoError(t, registrationErr)
	require.Len(t, registered.Status.Debug.Sessions[0].Processes, 2)
	registered.Status.Debug.Sessions[0].Processes[0].Name = "forged"
	registered.Status.Debug.Sessions[0].Processes[0].Observation = apiv2.ProjectDebugProcessObservation{Phase: apiv2.ProjectDebugProcessRunning}
	require.NoError(t, apiClient.Status().Update(ctx, registered))
	session.Phase = apiv2.ProjectDebugPhaseExited
	completed, completionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
	require.NoError(t, completionErr)
	require.Len(t, completed.Status.Debug.Sessions[0].Processes, 2)
	require.Equal(t, "target", completed.Status.Debug.Sessions[0].Processes[0].Name)
	require.Equal(t, apiv2.ProjectDebugProcessRunning, completed.Status.Debug.Sessions[0].Processes[0].Observation.Phase)
	completed.Spec.Stop = true
	require.NoError(t, apiClient.Update(ctx, completed))
	late := &apiv2.ProjectDebugProcessStatus{ID: "late", TrackingError: "local PID unavailable"}
	stopping, stoppingErr := writer.registerProcess(ctx, project.NamespacedName(), project.UID, session.ID, late)
	require.NoError(t, stoppingErr)
	require.Len(t, stopping.Status.Debug.Sessions[0].Processes, 3)
	require.True(t, stopping.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	_, wrongUIDErr := writer.registerProcess(ctx, project.NamespacedName(), "replacement-uid", session.ID, late)
	require.True(t, apierrors.IsConflict(wrongUIDErr))
}

// Verifies that failed process registration rolls back launched targets but never stops
// targets reported as attach or attachForSuspendedLaunch.
func TestProjectDebugRegistrationFailurePreservesAttachedTargets(t *testing.T) {
	for _, method := range []string{"launch", "attach", "attachForSuspendedLaunch"} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			apiClient, writer := startProjectDebugStatusServer(t, ctx)
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "registration-failure"},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{
					LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"},
				}}}
			require.NoError(t, apiClient.Create(ctx, project))
			session := &apiv2.ProjectDebugSessionStatus{ID: "session", Phase: apiv2.ProjectDebugPhaseRunning}
			_, sessionErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, session)
			require.NoError(t, sessionErr)
			executor := internaltest.NewTestProcessExecutor(ctx)
			defer executor.Dispose()
			handle, startWait, startErr := executor.StartProcess(ctx, exec.Command("reported-target"), nil, 0, nil)
			require.NoError(t, startErr)
			startWait()
			debugRuntime := newProjectDebugRuntime(ctx, executor, writer, logr.Discard())
			project.UID = "stale-project-uid"
			var message dap.Message
			require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(
				`{"type":"event","event":"process","body":{"systemProcessId":%d,"startMethod":%q}}`, handle.Pid, method)), &message))
			require.True(t, apierrors.IsConflict(debugRuntime.observeProcessMessage(project, session.ID, message)))
			runningErr := executor.CheckProcessRunning(handle)
			if method == "launch" {
				require.True(t, process.IsProcessGoneErr(runningErr))
			} else {
				require.NoError(t, runningErr)
			}
		})
	}
}

// Verifies that process events capture all local targets before forwarding, deduplicate repeated
// events and retain unconfirmed remote/missing identities without failing a live debug session.
func TestProjectDebugServingProcessEvents(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	connection, connectionErr := connectDebugServing(ctx, config, project)
	require.NoError(t, connectionErr)
	defer connection.Close()
	executions := executor.FindAll([]string{"test-adapter"}, "", nil)
	require.Len(t, executions, 1)
	body := `{"seq":42,"type":"request","command":"launch","arguments":{}}`
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
	_, _, echoErr := connection.ReadMessage()
	require.NoError(t, echoErr)
	require.NoError(t, dap.WriteMessage(executions[0].Cmd.Stdout, dap.Message{
		"seq": json.RawMessage("1"), "type": json.RawMessage(`"response"`),
		"command": json.RawMessage(`"launch"`), "request_seq": json.RawMessage("1"), "success": json.RawMessage("true"),
	}))
	_, _, launchReadErr := connection.ReadMessage()
	require.NoError(t, launchReadErr)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.True(t, project.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	var handles []process.ProcessHandle
	for range 2 {
		handle, wait, startErr := executor.TestProcessExecutor.StartProcess(ctx, exec.Command("reported-target"), nil, 0, nil)
		require.NoError(t, startErr)
		wait()
		handles = append(handles, handle)
	}
	sendEvent := func(body string) {
		var event dap.Message
		require.NoError(t, json.Unmarshal([]byte(`{"seq":50,"type":"event","event":"process","body":`+body+`}`), &event))
		require.NoError(t, dap.WriteMessage(executions[0].Cmd.Stdout, event))
		_, _, readErr := connection.ReadMessage()
		require.NoError(t, readErr)
	}
	sendEvent(fmt.Sprintf(`{"name":"first","systemProcessId":%d}`, handles[0].Pid))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.False(t, project.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	sendEvent(fmt.Sprintf(`{"name":"duplicate","systemProcessId":%d}`, handles[0].Pid))
	sendEvent(fmt.Sprintf(`{"name":"fork","systemProcessId":%d}`, handles[1].Pid))
	sendEvent(`{"name":"remote","systemProcessId":999,"isLocalProcess":false}`)
	sendEvent(`{"name":"unknown"}`)
	sendEvent(`{"name":"unavailable","systemProcessId":999}`)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Sessions[0].Processes, 5)
	require.Equal(t, apiv2.ProjectDebugPhaseRunning, project.Status.Debug.Sessions[0].Phase)
	require.True(t, project.Status.Debug.Sessions[0].ProcessTrackingUnconfirmed)
	require.WithinDuration(t, handles[0].IdentityTime, project.Status.Debug.Sessions[0].Processes[0].IdentityTimestamp.Time, time.Microsecond)
	closeDebugServing(t, connection)
	require.NoError(t, executor.CheckProcessRunning(handles[0]), "ending an adapter session must not erase target ownership")
}

// Verifies that malformed process-observation fields fail explicitly rather than hiding tracking failures.
func TestProjectDebugRejectsMalformedProcessMessages(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"type":true}`,
		`{"type":"response","command":true}`,
		`{"type":"response","command":"launch","success":"yes"}`,
		`{"type":"event","event":true}`,
		`{"type":"event","event":"process","body":{"systemProcessId":"invalid"}}`,
		`{"type":"event","event":"process","body":{"startMethod":true}}`,
	} {
		var message dap.Message
		require.NoError(t, json.Unmarshal([]byte(payload), &message))
		debugRuntime := &projectDebugRuntime{}
		require.Error(t, debugRuntime.observeProcessMessage(&apiv2.Project{}, "session", message))
	}
}
