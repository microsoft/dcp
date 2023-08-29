package integration_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxUpdatedAttempts = 5
)

func TestExecutableReplicaSetScales(t *testing.T) {
	const scaleTo = 3

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-scales",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: 0,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-scales",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	if err := updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		require.Equal(t, int32(0), ers.Status.ObservedReplicas)
		ers.Spec.Replicas = int32(scaleTo)
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale up: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	if err := updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		ers.Spec.Replicas = 0
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale down: %v", err)
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not scale back to zero Executables")
}

func TestExecutableReplicaSetRecreatesDeletedReplicas(t *testing.T) {
	const scaleTo = 3

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-recreates-replicas",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: int32(scaleTo),
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-restores",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executable replicas")

	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")

	oldUid := ownedExes[0].UID

	if err := client.Delete(ctx, ownedExes[0], ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("could not delete Executable: %v", err)
	}

	ensureReplicasRecreated := func(ctx context.Context) (bool, error) {
		newOwnedExes, err := getOwnedExes(ctx, &exers)
		if err != nil {
			t.Fatalf("Unable to fetch Executables: %v", err)
			return false, err
		}

		if len(newOwnedExes) != scaleTo {
			return false, nil
		}

		if slices.LenIf[*apiv1.Executable](newOwnedExes, func(exe *apiv1.Executable) bool {
			return exe.UID == oldUid
		}) != 0 {
			return false, nil
		}

		return true, nil
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureReplicasRecreated)
	require.NoError(t, err, "ExecutableReplicaSet did not recover to the expected number of Executable replicas")
}

func TestExecutableReplicaSetTemplateChangeAppliesToNewReplicas(t *testing.T) {
	const initialScaleTo = 2

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-template-changes",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: initialScaleTo,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-initial-template",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, initialScaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	if err := updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		ers.Spec.Replicas = initialScaleTo + 1
		ers.Spec.Template.Spec.ExecutablePath = "path/to/executable-replica-set-updated-template"
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet: %v", err)
	}

	ensureTemplateChangeReflected := func(ctx context.Context) (bool, error) {
		newOwnedExes, err := getOwnedExes(ctx, &exers)
		if err != nil {
			t.Fatalf("Unable to fetch Executables: %v", err)
			return false, err
		}

		if len(newOwnedExes) != initialScaleTo+1 {
			return false, nil
		}

		if slices.LenIf[*apiv1.Executable](newOwnedExes, func(exe *apiv1.Executable) bool {
			return exe.Spec.ExecutablePath == "path/to/executable-replica-set-initial-template"
		}) != initialScaleTo {
			return false, nil
		}

		if slices.LenIf[*apiv1.Executable](newOwnedExes, func(exe *apiv1.Executable) bool {
			return exe.Spec.ExecutablePath == "path/to/executable-replica-set-updated-template"
		}) != 1 {
			return false, nil
		}

		return true, nil
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureTemplateChangeReflected)
	require.NoError(t, err, "ExecutableReplicaSet did not scale back to zero Executables")
}

func TestExecutableReplicaSetDeleteRemovesExecutables(t *testing.T) {
	const initialScaleTo = 2

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-delete",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: initialScaleTo,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-delete",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, initialScaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	ensureReplicaSetDeleted := func(ctx context.Context) (bool, error) {
		if err := client.Delete(ctx, &exers); err != nil {
			t.Fatalf("Unable to delete ExecutableReplicaSet: %v", err)
			return false, err
		}

		return true, nil
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureReplicaSetDeleted)
	require.NoError(t, err, "ExecutableReplicaSet was not deleted")

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not scale back to zero Executables")
}

func TestExecutableReplicaSetExecutableStatusChangeTracked(t *testing.T) {
	const scaleTo = 3

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-executable-status-tracked",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: int32(scaleTo),
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-executable-status-tracked",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executable replicas")

	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")

	t.Log("Ensure first executable is started...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ownedExes[0]), func(exe *apiv1.Executable) (bool, error) {
		return !exe.Status.StartupTimestamp.IsZero(), nil
	})

	pid, err := strconv.ParseInt(updatedExe.Status.ExecutionID, 10, 32)
	require.NoError(t, err, "Failed to parse PID from Executable status")
	if err := processExecutor.StopProcess(int32(pid)); err != nil {
		t.Fatalf("could not kill process: %v", err)
	}

	ensureStatusUpdated := func(ctx context.Context) (bool, error) {
		var updatedExers apiv1.ExecutableReplicaSet
		if err := client.Get(ctx, ctrl_client.ObjectKeyFromObject(&exers), &updatedExers); err != nil {
			t.Fatalf("Unable to fetch updated ExecutableReplicaSet: %v", err)
			return false, err
		}

		newOwnedExes, err := getOwnedExes(ctx, &exers)
		if err != nil {
			t.Fatalf("Unable to fetch Executables: %v", err)
			return false, err
		}

		if len(newOwnedExes) != scaleTo || updatedExers.Status.ObservedReplicas != scaleTo {
			return false, nil
		}

		if updatedExers.Status.RunningReplicas != scaleTo-1 {
			return false, nil
		}

		if updatedExers.Status.FinishedReplicas != 1 {
			return false, nil
		}

		return true, nil
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureStatusUpdated)
	require.NoError(t, err, "ExecutableReplicaSet did not update its status to reflect the running replicas")
}

func ensureExpectedReplicaCount(t *testing.T, exers *apiv1.ExecutableReplicaSet, n int) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		ownedExes, err := getOwnedExes(ctx, exers)
		if err != nil {
			t.Fatalf("Unable to fetch Executables: %v", err)
			return false, err
		}

		if len(ownedExes) != n {
			return false, nil
		}

		return true, nil
	}
}

func getOwnedExes(ctx context.Context, exers *apiv1.ExecutableReplicaSet) ([]*apiv1.Executable, error) {
	var exes apiv1.ExecutableList
	if err := client.List(ctx, &exes); err != nil {
		return nil, err
	}

	var ownedExes []*apiv1.Executable = make([]*apiv1.Executable, 0)
	for i, exe := range exes.Items {
		if owner := metav1.GetControllerOf(&exe); owner != nil && owner.APIVersion == apiv1.GroupVersion.String() && owner.Kind == "ExecutableReplicaSet" && owner.Name == exers.Name {
			ownedExes = append(ownedExes, &exes.Items[i])
		}
	}

	return ownedExes, nil
}

func updateExecutableReplicaSet(ctx context.Context, key ctrl_client.ObjectKey, applyChanges func(*apiv1.ExecutableReplicaSet) error) error {
	var lastError error = nil

	for attempt := 0; attempt < maxUpdatedAttempts; attempt++ {
		var exers apiv1.ExecutableReplicaSet
		if err := client.Get(ctx, key, &exers); err != nil {
			lastError = err
			time.Sleep(time.Second)
			continue
		}

		patch := ctrl_client.MergeFromWithOptions(exers.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		if err := applyChanges(&exers); err != nil {
			lastError = err
			time.Sleep(time.Second)
			continue
		}

		if err := client.Patch(ctx, &exers, patch); err != nil {
			lastError = err
			time.Sleep(time.Second)
			continue
		}

		return nil
	}

	return fmt.Errorf("update failed, last error was %w", lastError)
}
