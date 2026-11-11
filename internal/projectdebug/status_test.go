/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeserverfilters "k8s.io/apiserver/pkg/server/filters"
	clientrest "k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/logs/v2resourcelogs"
	"github.com/microsoft/dcp/pkg/generated/openapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func startProjectDebugStatusServer(
	t *testing.T, ctx context.Context, configure ...func(*tiltapiserver.Config, *projectDebugStatusWriter),
) (ctrlclient.WithWatch, *projectDebugStatusWriter) {
	t.Helper()
	serverBuilder := builder.NewServerBuilder().
		WithResourceMemoryStorage(&apiv2.Project{}, "data").
		WithResourceMemoryStorage(&apiv2.Namespace{}, "data").
		WithOpenAPIDefinitions("debug-status-test", "1", openapi.GetOpenAPIDefinitions).
		WithBearerToken("debug-status-test-token")
	for _, additional := range apiv2.AdditionalTypes {
		serverBuilder.WithResource(additional)
	}
	options, optionsErr := serverBuilder.ToServerOptions()
	require.NoError(t, optionsErr)
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	t.Cleanup(func() { _ = listener.Close() })
	options.ServingOptions.Listener = listener
	options.ServingOptions.BindAddress = net.ParseIP("127.0.0.1")
	options.ServingOptions.BindPort = listener.Addr().(*net.TCPAddr).Port
	config, configErr := options.Config()
	require.NoError(t, configErr)
	config.GenericConfig.OpenAPIConfig.IgnorePrefixes = append(config.GenericConfig.OpenAPIConfig.IgnorePrefixes,
		"/apis/"+apiv2.GroupVersion.String()+"/projects/"+apiv2.LogSubresourceName)
	config.GenericConfig.LongRunningFunc = kubeserverfilters.BasicLongRunningRequestCheck(
		sets.NewString("watch"), sets.NewString(apiv2.DebugSubresourceName))
	apiv2.ResourceLogStreamers.Store((&apiv2.Project{}).GetGroupVersionResource(),
		v2resourcelogs.NewLogStreamer(testutil.NewLogForTesting(t.Name())))
	writer := &projectDebugStatusWriter{}
	require.NoError(t, configureProjectDebugStatus(config, writer))
	for _, apply := range configure {
		apply(config, writer)
	}
	serverCtx, cancelServer := context.WithCancel(ctx)
	t.Cleanup(cancelServer)
	stopped, startErr := options.RunTiltServerFromConfig(config.Complete(), serverCtx)
	require.NoError(t, startErr)
	t.Cleanup(func() {
		cancelServer()
		<-stopped
	})
	apiClient, clientErr := ctrlclient.NewWithWatch(clientrest.CopyConfig(config.GenericConfig.LoopbackClientConfig),
		ctrlclient.Options{Scheme: dcpclient.NewScheme()})
	require.NoError(t, clientErr)
	return apiClient, writer
}

// Verifies that Project debug observations are server-owned, survive ordinary status/parent writes,
// preserve application status and sibling entries, and retain normal resource-version conflicts
// while HTTP status responses and watch events are serialized concurrently.
func TestProjectDebugStatusIsolatesWriterOwnership(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "debug-status"},
		Spec:   apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}},
		Status: apiv2.ProjectStatus{Debug: &apiv2.ProjectDebugStatus{Sessions: []apiv2.ProjectDebugSessionStatus{{ID: "forged"}}}}}
	require.NoError(t, apiClient.Create(ctx, project))
	require.Nil(t, project.Status.Debug)
	project.Status.Phase = apiv2.ProjectPhaseReady
	require.NoError(t, apiClient.Status().Update(ctx, project))
	stale := project.DeepCopy()
	watcher, watchErr := apiClient.Watch(ctx, &apiv2.ProjectList{}, ctrlclient.InNamespace(project.Namespace))
	require.NoError(t, watchErr)
	defer watcher.Stop()
	first := &apiv2.ProjectDebugSessionStatus{ID: "first", Phase: apiv2.ProjectDebugPhasePending}
	execution := &apiv2.ProjectDebugExecutionStatus{ID: "provider", Phase: apiv2.ProjectDebugPhaseRunning}
	updated, publishErr := writer.update(ctx, project.NamespacedName(), project.UID, execution, first)
	require.NoError(t, publishErr)
	require.Equal(t, apiv2.ProjectPhaseReady, updated.Status.Phase)
	for observedUpdate := false; !observedUpdate; {
		select {
		case event, open := <-watcher.ResultChan():
			require.True(t, open)
			observed, isProject := event.Object.(*apiv2.Project)
			require.True(t, isProject, "%T", event.Object)
			if observed.ResourceVersion == updated.ResourceVersion {
				require.Equal(t, updated.Status.Debug, observed.Status.Debug)
				observedUpdate = true
			}
		case <-ctx.Done():
			t.Fatal("debug observation update did not reach the resource watch")
		}
	}
	stale.Status.Phase = apiv2.ProjectPhaseStopped
	require.True(t, apierrors.IsConflict(apiClient.Status().Update(ctx, stale)))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Status.Phase = apiv2.ProjectPhasePending
	project.Status.Debug = nil
	require.NoError(t, apiClient.Status().Update(ctx, project))
	require.Len(t, project.Status.Debug.Executions, 1)
	require.Len(t, project.Status.Debug.Sessions, 1)
	require.Equal(t, apiv2.ProjectPhasePending, project.Status.Phase)
	second := &apiv2.ProjectDebugSessionStatus{ID: "second", ExecutionID: "provider", Phase: apiv2.ProjectDebugPhaseRunning}
	_, secondErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, second)
	require.NoError(t, secondErr)
	require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"phase":"Ready","debug":{"sessions":[{"id":"forged","phase":"Running"}]}}}`))))
	require.Equal(t, apiv2.ProjectPhaseReady, project.Status.Phase)
	require.Len(t, project.Status.Debug.Sessions, 2)
	// Use the status client for a patch so that the normal HTTP /status handler and wrapper execute.
	require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"phase":"Ready","debug":null}}`))))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Equal(t, apiv2.ProjectPhaseReady, project.Status.Phase)
	require.Len(t, project.Status.Debug.Sessions, 2)
	require.Equal(t, "first", project.Status.Debug.Sessions[0].ID)
	require.Equal(t, "second", project.Status.Debug.Sessions[1].ID)
	first.Phase, first.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.NowMicro()
	first.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ConnectionFailed",
		Message: "injected connection failure", LastTransitionTime: metav1.Now()}}
	_, failedErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, first)
	require.NoError(t, failedErr)
	_, repeatedErr := writer.update(ctx, project.NamespacedName(), project.UID, nil, first)
	require.NoError(t, repeatedErr)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Sessions, 2)
	require.Equal(t, apiv2.ProjectDebugPhaseFailed, project.Status.Debug.Sessions[0].Phase)
	require.Equal(t, apiv2.ProjectDebugPhaseRunning, project.Status.Debug.Sessions[1].Phase)
	require.Equal(t, apiv2.ProjectPhaseReady, project.Status.Phase)
	launchFailure := &apiv2.ProjectDebugExecutionStatus{
		ID: "failed-launch", Phase: apiv2.ProjectDebugPhaseFailed, FinishedAt: metav1.NowMicro(),
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "LaunchFailed",
			Message: "injected adapter launch failure", LastTransitionTime: metav1.Now()}},
	}

	_, launchFailureErr := writer.update(ctx, project.NamespacedName(), project.UID, launchFailure, nil)
	require.NoError(t, launchFailureErr)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Executions, 2)
	require.Nil(t, project.Status.Debug.Executions[1].PID)
	require.Nil(t, project.Status.Debug.Executions[1].ExitCode)
	require.Equal(t, apiv2.ProjectPhaseReady, project.Status.Phase)
	project.Spec.Stop = true
	project.Status.Debug = nil
	require.NoError(t, apiClient.Update(ctx, project))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Sessions, 2)
}

// Verifies that internal API-owned observation updates are published through the normal Project API watch.
func TestProjectDebugStatusPublishesWatchEvents(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "debug-watch"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}}}
	require.NoError(t, apiClient.Create(ctx, project))
	watcher, watchErr := apiClient.Watch(ctx, &apiv2.ProjectList{}, ctrlclient.InNamespace(project.Namespace))
	require.NoError(t, watchErr)
	defer watcher.Stop()
	updated, publishErr := writer.update(ctx, project.NamespacedName(), project.UID, nil,
		&apiv2.ProjectDebugSessionStatus{ID: "first", Phase: apiv2.ProjectDebugPhasePending})
	require.NoError(t, publishErr)
	for {
		select {
		case event, open := <-watcher.ResultChan():
			require.True(t, open)
			observed, isProject := event.Object.(*apiv2.Project)
			require.True(t, isProject, "%T", event.Object)
			if observed.ResourceVersion == updated.ResourceVersion {
				require.Equal(t, updated.Status.Debug, observed.Status.Debug)
				return
			}
		case <-ctx.Done():
			t.Fatal("debug observation update did not reach the resource watch")
		}
	}
}

// Verifies that concurrent debug-entry and controller status updates merge at the actual storage boundary,
// without lost siblings or debug history erased by full controller status replacement.
func TestProjectDebugStatusConcurrentUpdates(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "debug-concurrent"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}}}
	require.NoError(t, apiClient.Create(ctx, project))
	const sessions = 12
	outcomes := make(chan error, sessions+1)
	var work sync.WaitGroup
	for i := range sessions {
		work.Add(1)
		go func(index int) {
			defer work.Done()
			_, publishErr := writer.update(ctx, project.NamespacedName(), project.UID, nil,
				&apiv2.ProjectDebugSessionStatus{ID: fmt.Sprintf("session-%d", index), Phase: apiv2.ProjectDebugPhaseRunning})
			outcomes <- publishErr
		}(i)
	}
	work.Add(1)
	go func() {
		defer work.Done()
		statusErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &apiv2.Project{}
			getErr := apiClient.Get(ctx, project.NamespacedName(), current)
			if getErr != nil {
				return getErr
			}
			current.Status = apiv2.ProjectStatus{Phase: apiv2.ProjectPhaseReady}
			return apiClient.Status().Update(ctx, current)
		})
		outcomes <- statusErr
	}()
	work.Wait()
	close(outcomes)
	for updateErr := range outcomes {
		require.NoError(t, updateErr)
	}
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Sessions, sessions)
	require.Equal(t, apiv2.ProjectPhaseReady, project.Status.Phase)
}

// Verifies that deleting Projects accept completion of existing debug history but reject new entries,
// and delayed observations cannot mutate a same-name replacement Project.
func TestProjectDebugStatusDeletionAndReplacement(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "debug-delete", Finalizers: []string{"test/retain"}},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}}}
	require.NoError(t, apiClient.Create(ctx, project))
	oldUID := project.UID
	session := &apiv2.ProjectDebugSessionStatus{ID: "existing", Phase: apiv2.ProjectDebugPhaseRunning}
	_, initialErr := writer.update(ctx, project.NamespacedName(), oldUID, nil, session)
	require.NoError(t, initialErr)
	require.NoError(t, apiClient.Delete(ctx, project))
	_, admissionErr := writer.update(ctx, project.NamespacedName(), oldUID, nil,
		&apiv2.ProjectDebugSessionStatus{ID: "new", Phase: apiv2.ProjectDebugPhasePending})
	require.True(t, apierrors.IsForbidden(admissionErr), "%v", admissionErr)
	session.Phase, session.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
	_, completionErr := writer.update(ctx, project.NamespacedName(), oldUID, nil, session)
	require.NoError(t, completionErr)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Finalizers = nil
	require.NoError(t, apiClient.Update(ctx, project))
	_, missingErr := writer.update(ctx, project.NamespacedName(), oldUID, nil, session)
	require.True(t, apierrors.IsNotFound(missingErr), "%v", missingErr)
	replacement := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: project.Name, Namespace: project.Namespace}, Spec: *project.Spec.DeepCopy()}
	require.NoError(t, apiClient.Create(ctx, replacement))
	_, staleErr := writer.update(ctx, replacement.NamespacedName(), oldUID, nil, session)
	require.True(t, apierrors.IsConflict(staleErr), "%v", staleErr)
	require.NoError(t, apiClient.Get(ctx, replacement.NamespacedName(), replacement))
	require.Nil(t, replacement.Status.Debug)
}
