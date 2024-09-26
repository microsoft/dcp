package integration_test

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
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

	t.Logf("scaling up replicas for ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		require.Equal(t, int32(0), ers.Status.ObservedReplicas)
		ers.Spec.Replicas = int32(scaleTo)
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale up: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	// Quickly check the displayName annotation to ensure it is set
	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")
	for _, exe := range ownedExes {
		require.NotEmpty(t, exe.Annotations[controllers.ExecutableDisplayNameAnnotation], "Executable '%s' does not have the expected display name annotation", exe.ObjectMeta.Name)
	}

	t.Logf("scaling down replicas for ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err = updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		ers.Spec.Replicas = 0
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale down: %v", err)
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not scale back to zero Executables")
}

func TestExecutableReplicaSetSoftDeleteScales(t *testing.T) {
	const scaleTo = 3

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-soft-delete-scales",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas:        0,
			StopOnScaleDown: true,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-soft-delete-scales",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	t.Logf("scaling up replicas for ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		require.Equal(t, int32(0), ers.Status.ObservedReplicas)
		ers.Spec.Replicas = int32(scaleTo)
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale up: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	t.Logf("scaling down replicas for ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err = updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		ers.Spec.Replicas = 0
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet to scale down: %v", err)
	}

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0, scaleTo))
	require.NoError(t, err, "ExecutableReplicaSet did not scale to the expected number of active and inactive Executables")
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

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executable replicas")

	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")

	exeToDelete := ownedExes[0]
	oldUid := exeToDelete.UID

	t.Logf("Deleting replica '%s'", exeToDelete.ObjectMeta.Name)
	err = retryOnConflict(ctx, exeToDelete.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
		return client.Delete(ctx, exeToDelete, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
	})
	if err != nil {
		t.Fatalf("could not delete Executable: %v", err)
	}

	t.Logf("Waiting for ExecutableReplicaSet '%s' to recreate the deleted replica", exers.ObjectMeta.Name)
	ensureReplicasRecreated := func(ctx context.Context) (bool, error) {
		newOwnedExes, ownedExesQueryErr := getOwnedExes(ctx, &exers)
		if ownedExesQueryErr != nil {
			t.Fatalf("Unable to fetch Executables: %v", ownedExesQueryErr)
			return false, ownedExesQueryErr
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

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, initialScaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	if err = updateExecutableReplicaSet(ctx, ctrl_client.ObjectKeyFromObject(&exers), func(ers *apiv1.ExecutableReplicaSet) error {
		ers.Spec.Replicas = initialScaleTo + 1
		ers.Spec.Template.Spec.ExecutablePath = "path/to/executable-replica-set-updated-template"
		return nil
	}); err != nil {
		t.Fatalf("could not update ExecutableReplicaSet: %v", err)
	}

	ensureTemplateChangeReflected := func(ctx context.Context) (bool, error) {
		newOwnedExes, ownedExesQueryErr := getOwnedExes(ctx, &exers)
		if ownedExesQueryErr != nil {
			t.Fatalf("Unable to fetch Executables: %v", ownedExesQueryErr)
			return false, ownedExesQueryErr
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

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, initialScaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	err = retryOnConflict(ctx, exers.NamespacedName(), func(ctx context.Context, currentExers *apiv1.ExecutableReplicaSet) error {
		return client.Delete(ctx, currentExers)
	})
	require.NoError(t, err, "ExecutableReplicaSet was not deleted")

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not scale back to zero Executables")
}

func TestExecutableReplicaSetDeleteRemovesSoftDeleteExecutables(t *testing.T) {
	const initialScaleTo = 2

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-delete-with-soft-delete",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas:        initialScaleTo,
			StopOnScaleDown: true,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-delete-with-soft-delete",
				},
			},
		},
	}

	t.Logf("creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, initialScaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	err = retryOnConflict(ctx, exers.NamespacedName(), func(ctx context.Context, currentExers *apiv1.ExecutableReplicaSet) error {
		return client.Delete(ctx, currentExers)
	})
	require.NoError(t, err, "ExecutableReplicaSet was not deleted")

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, 0, 0))
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

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executable replicas")

	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")

	t.Log("Ensure first Executable is started...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ownedExes[0]), func(exe *apiv1.Executable) (bool, error) {
		return !exe.Status.StartupTimestamp.IsZero(), nil
	})

	t.Log("Stopping first Executable...")
	pid64, err := strconv.ParseInt(updatedExe.Status.ExecutionID, 10, 32)
	require.NoError(t, err, "Failed to parse PID from Executable status")
	pid, err := process.Int64ToPidT(pid64)
	require.NoError(t, err)
	testProcessExecutor.SimulateProcessExit(t, pid, 0)

	// Replica running to completion does not trigger creation of another replica,
	// so we expect (scaleTo - 1) replicas to be running and one to be finished.
	ensureStatusUpdated := func(ctx context.Context) (bool, error) {
		var updatedExers apiv1.ExecutableReplicaSet
		updatedExersQueryErr := client.Get(ctx, ctrl_client.ObjectKeyFromObject(&exers), &updatedExers)
		if updatedExersQueryErr != nil {
			t.Fatalf("unable to fetch updated ExecutableReplicaSet: %v", updatedExersQueryErr)
			return false, updatedExersQueryErr
		}

		if updatedExers.Status.ObservedReplicas != scaleTo {
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

func TestExecutableReplicaSetInjectsPortsIntoReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "exers-injects-ports-into-replicas"
	const replicas = 3

	// In the current implementation the service must exist before the Executable is created

	svcAName := testName + "-svc-a"
	svcA := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcAName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  networking.IPv4LocalhostDefaultAddress,
			Port:     13762,
		},
	}

	t.Logf("Creating Service '%s'", svcAName)
	err := client.Create(ctx, &svcA)
	require.NoError(t, err, "Could not create Service '%s'", svcAName)

	svcBName := testName + "-svc-b"
	svcB := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcBName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  networking.IPv4LocalhostDefaultAddress,
			Port:     13763,
		},
	}

	t.Logf("Creating Service '%s'", svcBName)
	err = client.Create(ctx, &svcB)
	require.NoError(t, err, "Could not create Service '%s'", svcBName)

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: replicas,
			Template: apiv1.ExecutableTemplate{
				Annotations: map[string]string{
					"service-producer": fmt.Sprintf(`[{"serviceName":"%s"}, {"serviceName":"%s"}]`, svcAName, svcBName),
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/exers-injects-ports-into-replicas",
					Env: []apiv1.EnvVar{
						{
							Name:  "SVC_PORT",
							Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, svcAName),
						},
					},
					Args: []string{
						fmt.Sprintf(`--svc-b-port={{- portForServing "%s" -}}`, svcBName),
					},
				},
			},
		},
	}

	t.Logf("Creating ExecutableReplicaSet '%s'...", exers.ObjectMeta.Name)
	err = client.Create(ctx, &exers)
	require.NoError(t, err, "Could not create the ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)

	t.Logf("Ensure ExecutableReplicaSet '%s' has expected number of replicas...", exers.ObjectMeta.Name)
	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, replicas, 0))
	require.NoError(t, err, "ExecutableReplicaSet did not create the expected number of Executables")

	t.Logf("Ensure ExecutableReplicaSet '%s' has injected ports into replicas...", exers.ObjectMeta.Name)
	ownedExes, err := getOwnedExes(ctx, &exers)
	require.NoError(t, err, "Failed to retrieve owned Executable replicas")

	portMap := make(map[int32]bool, replicas)
	validateAndParsePortStr := func(portStr string) int32 {
		port, portParsingErr := strconv.ParseInt(portStr, 10, 32)
		require.NoError(t, portParsingErr, "The injected port '%s' is not a valid integer", portStr)
		require.Greater(t, int32(port), int32(0), "The injected port '%d' is not a valid port number", port)
		require.Less(t, int32(port), int32(65536), "The injected port '%d' is not a valid port number", port)
		return int32(port)
	}
	svcBPortRegex := regexp.MustCompile(`--svc-b-port=(\d+)`)

	for _, exe := range ownedExes {
		updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(updatedExe *apiv1.Executable) (bool, error) {
			return updatedExe.Status.State == apiv1.ExecutableStateRunning, nil
		})

		effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
			return fmt.Sprintf("%s=%s", v.Name, v.Value)
		})

		matches := testutil.FindAllMatching(effectiveEnv, regexp.MustCompile(`SVC_PORT=(\d+)`))
		require.Len(t, matches, 1, "Port was not injected into the Executable '%s',process environment variables. The process environment variables are: %v", updatedExe.ObjectMeta.Name, effectiveEnv)
		port := validateAndParsePortStr(matches[0][1])

		_, alreadySeen := portMap[port]
		require.False(t, alreadySeen, "The port '%d' injected into Executable '%s' is not unique", port, exe.ObjectMeta.Name)
		portMap[port] = true

		matches = testutil.FindAllMatching(updatedExe.Status.EffectiveArgs, svcBPortRegex)
		require.Len(t, matches, 1, "Port for service B was not injected into the Executable '%s' startup parameters. The startup parameters are: %v", exe.ObjectMeta.Name, updatedExe.Status.EffectiveArgs)
		svcDPortStr := matches[0][1]
		validateAndParsePortStr(svcDPortStr)
	}
}

func TestExecutableReplicaSetHealthStatus(t *testing.T) {
	const scaleTo = 3

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exers := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-replica-set-health-status",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: int32(scaleTo),
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-replica-set-health-status",
				},
			},
		},
	}

	t.Logf("Creating ExecutableReplicaSet '%s'", exers.ObjectMeta.Name)
	if err := client.Create(ctx, &exers); err != nil {
		t.Fatalf("could not create ExecutableReplicaSet: %v", err)
	}
	replicaErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, replicaErr, "ExecutableReplicaSet did not create the expected number of Executable replicas")

	t.Logf("Ensure ExecutableReplicaSet '%s' is healthy...", exers.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exers), func(updatedExers *apiv1.ExecutableReplicaSet) (bool, error) {
		return updatedExers.Status.HealthStatus == apiv1.HealthStatusHealthy, nil
	})

	ownedExes, getExeErr := getOwnedExes(ctx, &exers)
	require.NoError(t, getExeErr, "Failed to retrieve owned Executable replicas")

	stopExecutableProcess := func(exe *apiv1.Executable) {
		require.NotEmpty(t, exe.Status.ExecutionID, "Executable does not have an execution ID")
		pid64, parseErr := strconv.ParseInt(exe.Status.ExecutionID, 10, 32)
		require.NoError(t, parseErr, "Failed to parse PID from Executable status")
		pid, convertErr := process.Int64ToPidT(pid64)
		require.NoError(t, convertErr)
		testProcessExecutor.SimulateProcessExit(t, pid, 0)
	}

	t.Log("Stopping first Executable process...")
	stopExecutableProcess(ownedExes[0])

	t.Logf("Ensure ExecutableReplicaSet '%s' health status is 'Caution'...", exers.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exers), func(updatedExers *apiv1.ExecutableReplicaSet) (bool, error) {
		return updatedExers.Status.HealthStatus == apiv1.HealthStatusCaution, nil
	})

	t.Logf("Stopping remaining Executables...")
	for _, exe := range ownedExes[1:] {
		stopExecutableProcess(exe)
	}

	t.Logf("Ensure ExecutableReplicaSet '%s' health status is 'Unhealthy'...", exers.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exers), func(updatedExers *apiv1.ExecutableReplicaSet) (bool, error) {
		return updatedExers.Status.HealthStatus == apiv1.HealthStatusUnhealthy, nil
	})

	t.Logf("Deleting all existing Executables...")
	for _, exe := range ownedExes {
		deleteErr := retryOnConflict(ctx, exe.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
			return client.Delete(ctx, currentExe)
		})
		require.NoError(t, deleteErr, "Failed to delete Executable '%s'", exe.ObjectMeta.Name)
	}

	t.Logf("Waiting for replica set '%s' to create new replicas and become healthy again...", exers.ObjectMeta.Name)
	replicaErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ensureExpectedReplicaCount(t, &exers, scaleTo, 0))
	require.NoError(t, replicaErr, "ExecutableReplicaSet did not -recreate the expected number of Executable replicas")
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exers), func(updatedExers *apiv1.ExecutableReplicaSet) (bool, error) {
		return updatedExers.Status.HealthStatus == apiv1.HealthStatusHealthy, nil
	})
}

func ensureExpectedReplicaCount(t *testing.T, exers *apiv1.ExecutableReplicaSet, expectedActive int, expectedInactive int) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		ownedExes, err := getOwnedExes(ctx, exers)
		if err != nil {
			t.Fatalf("Unable to fetch Executables: %v", err)
			return false, err
		}

		active := slices.LenIf[*apiv1.Executable](ownedExes, func(exe *apiv1.Executable) bool {
			return exe.Annotations[controllers.ExecutableReplicaStateAnnotation] == string(controllers.ExecutableReplicaSetStateActive)
		})
		inactive := len(ownedExes) - active

		if active != expectedActive || inactive != expectedInactive {
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

func updateExecutableReplicaSet(ctx context.Context, name types.NamespacedName, applyChanges func(*apiv1.ExecutableReplicaSet) error) error {
	return retryOnConflict(ctx, name, func(ctx context.Context, currentExeRS *apiv1.ExecutableReplicaSet) error {
		patch := ctrl_client.MergeFromWithOptions(currentExeRS.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		if err := applyChanges(currentExeRS); err != nil {
			return err
		}
		return client.Patch(ctx, currentExeRS, patch)
	})
}
