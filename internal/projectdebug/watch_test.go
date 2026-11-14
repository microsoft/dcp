/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type debugWatchStorage struct {
	rest.StandardStorage
	gets        atomic.Int32
	watches     atomic.Int32
	initialRead func() error
	activeWatch watch.Interface
}

func (storage *debugWatchStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	if storage.gets.Add(1) != 1 {
		return nil, fmt.Errorf("unexpected repeated debug observation read")
	}
	if storage.initialRead != nil {
		if readErr := storage.initialRead(); readErr != nil {
			return nil, readErr
		}
	}
	return storage.StandardStorage.Get(ctx, name, options)
}

func (storage *debugWatchStorage) Watch(ctx context.Context, options *metainternal.ListOptions) (watch.Interface, error) {
	storage.watches.Add(1)
	watcher, watchErr := storage.StandardStorage.Watch(ctx, options)
	storage.activeWatch = watcher
	return watcher, watchErr
}

// Verifies that debug monitoring establishes both watches before its one-time identity read,
// consumes readiness and shutdown snapshots without rereads, and observes independent namespace changes.
func TestProjectDebugMonitorUsesWatchSnapshots(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"project-stop", "namespace-stop", "deleted-during-subscription", "project-watch-closed", "namespace-watch-closed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			var storage *projectDebugStorage
			var projects, namespaces *debugWatchStorage
			apiClient, _ := startProjectDebugStatusServer(t, ctx, func(config *tiltapiserver.Config, writer *projectDebugStatusWriter) {
				gvr := (&apiv2.Project{}).GetGroupVersionResource()
				parentProvider := config.ExtraConfig.APIs[gvr]
				namespaceProvider := config.ExtraConfig.APIs[(&apiv2.Namespace{}).GetGroupVersionResource()]
				gvr.Resource += "/" + apiv2.DebugSubresourceName
				config.ExtraConfig.APIs[gvr] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
					parent, parentErr := parentProvider(scheme, getter)
					if parentErr != nil {
						return nil, parentErr
					}
					namespaceStore, namespaceErr := namespaceProvider(scheme, getter)
					if namespaceErr != nil {
						return nil, namespaceErr
					}
					projects = &debugWatchStorage{StandardStorage: parent.(rest.StandardStorage)}
					namespaces = &debugWatchStorage{StandardStorage: namespaceStore.(rest.StandardStorage)}
					storage = newProjectDebugStorage(ctx, projects, namespaces, writer, process.NewOSExecutor(logr.Discard()), logr.Discard())
					return storage, nil
				}
			})
			project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
			project.Status.Conditions = nil
			require.NoError(t, apiClient.Status().Update(ctx, project))
			namespace := &apiv2.Namespace{}
			require.NoError(t, apiClient.Get(ctx, ctrlclient.ObjectKey{Name: project.Namespace}, namespace))
			namespace.Status.Phase = ""
			require.NoError(t, apiClient.Status().Update(ctx, namespace))
			projects.initialRead = func() error {
				if projects.watches.Load() != 1 || namespaces.watches.Load() != 1 {
					return errors.New("initial read overtook watch registration")
				}
				if scenario == "deleted-during-subscription" {
					return apiClient.Delete(ctx, project)
				}
				return nil
			}
			lifetime, cancelMonitor := context.WithCancelCause(ctx)
			defer cancelMonitor(nil)
			ready := make(chan *apiv2.Project, 1)
			done := make(chan struct{})
			selected := project.DeepCopy()
			go func() {
				defer close(done)
				storage.monitor(lifetime, selected, ready, cancelMonitor)
			}()
			t.Cleanup(func() { cancelMonitor(nil); <-done })
			if scenario == "deleted-during-subscription" {
				select {
				case <-done:
					require.Error(t, context.Cause(lifetime))
				case <-ctx.Done():
					t.Fatal("Project deletion during subscription was missed")
				}
				return
			}
			project.Status.Conditions = []metav1.Condition{{Type: string(apiv2.ConditionReady), Status: metav1.ConditionTrue,
				Reason: string(apiv2.ProjectReasonPrepared), LastTransitionTime: metav1.Now()}}
			require.NoError(t, apiClient.Status().Update(ctx, project))
			namespace.Status.Phase = apiv2.NamespacePhaseActive
			require.NoError(t, apiClient.Status().Update(ctx, namespace))
			select {
			case prepared := <-ready:
				require.Equal(t, project.UID, prepared.UID)
				require.Equal(t, string(apiv2.ProjectReasonPrepared), prepared.Status.Conditions[0].Reason)
			case <-lifetime.Done():
				t.Fatalf("watch readiness failed: %v", context.Cause(lifetime))
			}
			switch scenario {
			case "project-stop":
				require.NoError(t, apiClient.Patch(ctx, project, ctrlclient.RawPatch(
					types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			case "namespace-stop":
				namespace.Status.Phase = apiv2.NamespacePhaseTerminating
				require.NoError(t, apiClient.Status().Update(ctx, namespace))
			case "project-watch-closed":
				projects.activeWatch.Stop()
			case "namespace-watch-closed":
				namespaces.activeWatch.Stop()
			}
			select {
			case <-done:
				if scenario == "project-stop" || scenario == "namespace-stop" {
					require.ErrorIs(t, context.Cause(lifetime), dap.ErrShutdownRequested)
				} else {
					require.ErrorContains(t, context.Cause(lifetime), "watch closed")
				}
			case <-ctx.Done():
				t.Fatal("shutdown watch event was missed")
			}
			require.EqualValues(t, 1, projects.gets.Load())
			require.Zero(t, namespaces.gets.Load())
			require.EqualValues(t, 1, projects.watches.Load())
			require.EqualValues(t, 1, namespaces.watches.Load())
		})
	}
}
