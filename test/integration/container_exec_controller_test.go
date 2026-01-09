// Copyright (c) Microsoft Corporation. All rights reserved.

package integration_test

import (
	"testing"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// Ensure a ContainerExec record properly records completion when the command finishes.
func TestContainerExecRunsToCompletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-exec-runs-to-completion"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspect := ensureContainerRunning(t, ctx, &ctr)

	exec := apiv1.ContainerExec{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerExecSpec{
			ContainerName: testName,
			Command:       testName + "-command",
		},
	}

	t.Logf("Creating ContainerExec object '%s'", exec.ObjectMeta.Name)
	err = client.Create(ctx, &exec)
	require.NoError(t, err, "Could not create a ContainerExec object")

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exec), func(e *apiv1.ContainerExec) (bool, error) {
		return e.Status.State == apiv1.ExecutableStateRunning, nil
	})

	err = containerOrchestrator.SimulateContainerExecExit(ctx, inspect.Id, exec.Spec.Command, 0)
	require.NoError(t, err, "Could not simulate ContainerExec exit")

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exec), func(e *apiv1.ContainerExec) (bool, error) {
		return e.Status.State == apiv1.ExecutableStateFinished && e.Status.ExitCode != nil && *e.Status.ExitCode == 0, nil
	})
}

// Ensure a ContainerExec record properly records a non-zero exit code when the command finishes.
func TestContainerExecTracksExitCode(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-exec-tracks-exit-code"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspect := ensureContainerRunning(t, ctx, &ctr)

	exec := apiv1.ContainerExec{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerExecSpec{
			ContainerName: testName,
			Command:       testName + "-command",
		},
	}

	t.Logf("Creating ContainerExec object '%s'", exec.ObjectMeta.Name)
	err = client.Create(ctx, &exec)
	require.NoError(t, err, "Could not create a ContainerExec object")

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exec), func(e *apiv1.ContainerExec) (bool, error) {
		return e.Status.State == apiv1.ExecutableStateRunning, nil
	})

	err = containerOrchestrator.SimulateContainerExecExit(ctx, inspect.Id, exec.Spec.Command, 10)
	require.NoError(t, err, "Could not simulate ContainerExec exit")

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exec), func(e *apiv1.ContainerExec) (bool, error) {
		return e.Status.State == apiv1.ExecutableStateFinished && e.Status.ExitCode != nil && *e.Status.ExitCode == 10, nil
	})
}
