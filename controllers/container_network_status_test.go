/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type networkStatusOrchestrator struct {
	containers.ContainerOrchestrator
	inspected containers.InspectedContainer
}

func (orchestrator networkStatusOrchestrator) InspectContainers(_ context.Context, _ containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	return []containers.InspectedContainer{orchestrator.inspected}, nil
}

// Verifies that container state initializers persist the final network disconnect once and preserve network status on lookup failure.
func TestContainerStateNetworkStatus(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name         string
		state        apiv1.ContainerState
		runtimeState containers.ContainerStatus
		initialize   containerStateInitializerFunc
	}{
		{name: "running", state: apiv1.ContainerStateRunning, runtimeState: containers.ContainerStatusRunning, initialize: ensureContainerRunningState},
		{name: "paused", state: apiv1.ContainerStatePaused, runtimeState: containers.ContainerStatusPaused, initialize: ensureContainerRunningState},
		{name: "exited", state: apiv1.ContainerStateExited, runtimeState: containers.ContainerStatusExited, initialize: ensureContainerExitedState},
		{name: "failed to start", state: apiv1.ContainerStateFailedToStart, initialize: ensureContainerFailedToStartState},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			for _, lookupFails := range []bool{false, true} {
				t.Run(map[bool]string{false: "disconnected", true: "lookup failed"}[lookupFails], func(t *testing.T) {
					t.Parallel()
					ctx, cancel := testutil.GetTestContext(t, 0)
					defer cancel()
					log := logr.Discard()
					scheme := runtime.NewScheme()
					require.NoError(t, apiv1.AddToScheme(scheme))
					client := fake.NewClientBuilder().WithScheme(scheme).
						WithIndex(&apiv1.ContainerNetworkConnection{}, ownerKey, func(ctrl_client.Object) []string { return nil }).
						WithIndex(&apiv1.Endpoint{}, commonapi.WorkloadOwnerKey, func(ctrl_client.Object) []string { return nil }).
						WithInterceptorFuncs(interceptor.Funcs{
							List: func(listCtx context.Context, innerClient ctrl_client.WithWatch, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
								if _, isConnectionList := list.(*apiv1.ContainerNetworkConnectionList); isConnectionList && lookupFails {
									return errors.New("network lookup failed")
								}
								return innerClient.List(listCtx, list, options...)
							},
						}).Build()
					inspected := containers.InspectedContainer{Id: "container-id", Name: "container", Status: testCase.runtimeState}
					reconciler := &ContainerReconciler{
						ReconcilerBase: NewReconcilerBase[apiv1.Container](client, client, log, ctx),
						orchestrator:   networkStatusOrchestrator{inspected: inspected},
					}
					container := &apiv1.Container{
						ObjectMeta: metav1.ObjectMeta{Name: "container"},
						Spec:       apiv1.ContainerSpec{Image: "test-image", Networks: &[]apiv1.ContainerNetworkConnectionConfig{}},
					}
					data := newRunningContainerData(container)
					data.updateFromInspectedContainer(&inspected)
					data.containerState = testCase.state
					data.applyTo(container, log)
					container.Status.Networks = []string{"previous-network"}

					firstChange := testCase.initialize(ctx, reconciler, container, testCase.state, data, log)
					if lookupFails {
						require.Equal(t, additionalReconciliationNeeded, firstChange)
						require.Equal(t, []string{"previous-network"}, container.Status.Networks)
						return
					}
					require.Equal(t, statusChanged, firstChange)
					require.Empty(t, container.Status.Networks)
					require.Equal(t, noChange, testCase.initialize(ctx, reconciler, container, testCase.state, data, log))
				})
			}
		})
	}
}
