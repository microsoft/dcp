/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

type stopInspectionOrchestrator struct {
	containers.ContainerOrchestrator
	client             ctrl_client.Client
	key                ctrl_client.ObjectKey
	armed              atomic.Bool
	inspectionFailures atomic.Int32
	prematureStop      atomic.Bool
}

func (orchestrator *stopInspectionOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if orchestrator.armed.Load() {
		var container apiv1.Container
		if getErr := orchestrator.client.Get(ctx, orchestrator.key, &container); getErr != nil {
			return nil, getErr
		}
		if container.Spec.Stop && orchestrator.inspectionFailures.Load() < 3 {
			if container.Status.State != apiv1.ContainerStateRunning {
				orchestrator.prematureStop.Store(true)
			}
			orchestrator.inspectionFailures.Add(1)
			// Model an exhausted inspection attempt without waiting for the inner inspection backoff.
			return nil, resiliency.Permanent(fmt.Errorf("container runtime inspection is temporarily unavailable"))
		}
	}
	return orchestrator.ContainerOrchestrator.InspectContainers(ctx, options)
}

// Verifies that a stop request survives transient inspection failures and completes after runtime recovery.
// Reconciliation must resume without runtime events and retain Running until inspection succeeds.
func TestContainerStopRecoversAfterInspectionFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	var orchestrator *stopInspectionOrchestrator
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(ctx, ContainerController, t.Name(), t.TempDir(), TestEnvironmentOptions{
		DecorateContainerOrchestrator: func(inner containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			orchestrator = &stopInspectionOrchestrator{ContainerOrchestrator: inner}
			return orchestrator
		},
	})
	require.NoError(t, startupErr)
	container := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{Name: "stop-after-inspection-recovery"},
		Spec:       apiv1.ContainerSpec{Image: "stop-after-inspection-recovery-image"},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, container))
	_, _ = ensureContainerRunningEx(t, ctx, serverInfo.Client, orchestrator, container)
	orchestrator.client = serverInfo.Client
	orchestrator.key = ctrl_client.ObjectKeyFromObject(container)
	orchestrator.armed.Store(true)
	patchErr := retryOnConflictEx(ctx, serverInfo.Client, container.NamespacedName(), func(patchCtx context.Context, current *apiv1.Container) error {
		patch := current.DeepCopy()
		patch.Spec.Stop = true
		return serverInfo.Client.Patch(patchCtx, patch, ctrl_client.MergeFromWithOptions(current, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, patchErr)
	stopped := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, orchestrator.key, func(current *apiv1.Container) (bool, error) {
		return current.Status.State == apiv1.ContainerStateExited, nil
	})
	require.EqualValues(t, 3, orchestrator.inspectionFailures.Load())
	require.False(t, orchestrator.prematureStop.Load(), "stop must wait for runtime recovery")
	require.NotNil(t, stopped.Status.ExitCode)
	require.False(t, stopped.Status.FinishTimestamp.IsZero())
}
