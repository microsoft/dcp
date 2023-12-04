package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// Ensure a container instance is started when new Container object appears
func TestContainerInstanceStarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-instance-starts"
	const imageName = testName + "-image"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

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

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")
}

func TestContainerMarkedDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-marked-done"
	const imageName = testName + "-image"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	t.Log("Simulating container exit...")
	err = simulateContainerExit(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Could not simulate container exit")

	t.Log("Ensure Container object status reflects the state of the running container...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return !c.Status.FinishTimestamp.IsZero() && c.Status.State == apiv1.ContainerStateExited, nil
	})
}

func TestContainerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-startup-failure"
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

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	// Technically, we are simulation the failure of the orchestrator to start the container...
	t.Log("Simulating container startup failure...")
	dockerRunCmd, err := waitForDockerCommand(t, ctx, []string{"run"}, imageName)
	require.NoError(t, err)

	errMsg := fmt.Sprintf("Container '%s' could not be run because it is just a test :-)", imageName)
	_, err = dockerRunCmd.Cmd.Stderr.Write([]byte(errMsg))
	require.NoError(t, err)
	processExecutor.SimulateProcessExit(t, dockerRunCmd.PID, 1)

	// The container should be marked as "failed to start", and the Message property of its status
	// should contain the error from container orchestrator, i.e. Docker
	t.Log("Ensure container state is 'failed to start'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateFailedToStart
		messageOK := strings.Contains(c.Status.Message, errMsg)
		return statusUpdated && messageOK, nil
	})
}

// If ports are part of the spec, they are published to the host
func TestContainerStartWithPorts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// Case 1: just ContainerPort
	testName := "container-start-with-ports-case1"
	imageName := testName + "-image"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345},
				{ContainerPort: 3456},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure that the container is started with the expected ports published...")
	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, imageName, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.0.1::2345"}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.0.1::3456"}) == -1 {
			return false
		}
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	// Case 2: ContainerPort and HostPort
	testName = "container-start-with-ports-case2"
	imageName = testName + "-image"
	containerID = testName + "-" + testutil.GetRandLetters(t, 6)

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostPort: 8885},
				{ContainerPort: 3456, HostPort: 8886},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure that the container is started with the expected ports published...")
	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, imageName, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.0.1:8885:2345"}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.0.1:8886:3456"}) == -1 {
			return false
		}
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime = time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	// Case 3: ContainerPort and HostIP
	testName = "container-start-with-ports-case3"
	imageName = testName + "-image"
	containerID = testName + "-" + testutil.GetRandLetters(t, 6)

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostIP: "127.0.2.3"},
				{ContainerPort: 3456, HostIP: "127.0.2.4"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure that the container is started with the expected ports published...")
	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, imageName, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.2.3::2345"}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.2.4::3456"}) == -1 {
			return false
		}
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime = time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	// Case 4: ContainerPort, HostIP, and Portocol
	testName = "container-start-with-ports-case4"
	imageName = testName + "-image"
	containerID = testName + "-" + testutil.GetRandLetters(t, 6)

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostIP: "127.0.3.4", Protocol: "tcp"},
				{ContainerPort: 3456, HostIP: "127.0.4.4", Protocol: "udp"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure that the container is started with the expected ports published...")
	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, imageName, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.3.4::2345/tcp"}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.4.4::3456/udp"}) == -1 {
			return false
		}
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime = time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	// Case 5: ContainerPort, HostIP, HostPort, and Protocol
	testName = "container-start-with-ports-case5"
	imageName = testName + "-image"
	containerID = testName + "-" + testutil.GetRandLetters(t, 6)

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostPort: 12202, HostIP: "127.0.3.4", Protocol: "tcp"},
				{ContainerPort: 3456, HostPort: 12205, HostIP: "127.0.4.4", Protocol: "udp"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure that the container is started with the expected ports published...")
	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, imageName, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.3.4:12202:2345/tcp"}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", "127.0.4.4:12205:3456/udp"}) == -1 {
			return false
		}
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime = time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)
}

func TestContainerDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-deletion"
	const imageName = testName + "-image"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	ensureContainerDeletionResponse(t, containerID)

	t.Log("Deleting Container object...")
	err = client.Delete(ctx, &ctr)
	require.NoError(t, err, "Container object could not be deleted")

	t.Log("Check if corresponding container was deleted...")
	err = waitForDockerContainerRemoved(t, ctx, containerID)
	require.NoError(t, err, "Container was not deleted as expected")

	t.Log("Ensure that Container object really disappeared from the API server...")
	waitObjectDeleted[apiv1.Container](t, ctx, ctrl_client.ObjectKeyFromObject(&ctr))
}

func TestContainerParallelStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const containerCount = 4
	require.LessOrEqual(t, containerCount, controllers.MaxParallelContainerStarts, "This test is not designed to verify parallel start throttling beyond maxParallelContainerStarts")

	const containerNameFormat = "container-parallel-start-%d"
	const containerImageFormat = containerNameFormat + "-image"
	const containerIDFormat = containerNameFormat + "-%s"

	for i := 0; i < containerCount; i++ {
		ctr := apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf(containerNameFormat, i),
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ContainerSpec{
				Image: fmt.Sprintf(containerImageFormat, i),
			},
		}

		t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
		err := client.Create(ctx, &ctr)
		require.NoError(t, err, "Could not create a Container '%s'", ctr.ObjectMeta.Name)
	}

	t.Log("Ensure that all containers are in the process of being started...")
	for i := 0; i < containerCount; i++ {
		// Do not use ensureContainerRunning() just yet, because that one completes the container startup sequence
		// and will not allow us to verify that the containers are being started in parallel.
		// Just check that the appropriate "docker run" command was issued here.
		image := fmt.Sprintf(containerImageFormat, i)
		_, err := waitForDockerCommand(t, ctx, []string{"run"}, image)
		require.NoError(t, err, "Could not find the expected 'docker run' command for container '%s'", image)
	}

	t.Log("Complete the container startup sequences...")
	for i := 0; i < containerCount; i++ {
		err := ensureContainerRunning(t, ctx,
			fmt.Sprintf(containerImageFormat, i),
			fmt.Sprintf(containerIDFormat, i, testutil.GetRandLetters(t, 6)), time.Now().UTC(),
		)
		require.NoError(t, err, "Docker run command for Container '%s' was not issued as expected", fmt.Sprintf(containerNameFormat, i))
	}

	// We need to make sure the containers were actually started, and not just timed out on the start attempt.
	for i := 0; i < containerCount; i++ {
		containerName := fmt.Sprintf(containerNameFormat, i)
		t.Logf("Ensure Container '%s' is running...", containerName)
		containerKey := ctrl_client.ObjectKey{Name: containerName, Namespace: metav1.NamespaceNone}
		waitObjectAssumesState(t, ctx, containerKey, func(c *apiv1.Container) (bool, error) {
			return c.Status.State == apiv1.ContainerStateRunning, nil
		})
	}
}

func TestContainerRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// If a container shuts down and then restarts, it should be tracked as running
	const testName = "container-restart"
	const imageName = testName + "-image"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	err = simulateContainerExit(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Could not simulate container exit")

	t.Log("Ensure container state is 'stopped'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateExited
		return statusUpdated, nil
	})

	// Restart the container
	err = simulateContainerStart(t, ctx, ctr.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Could not simulate container start")

	t.Log("Ensure container state is 'running'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateRunning
		return statusUpdated, nil
	})
}

func TestContainerMultipleServingPortsInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const IPAddr = "127.22.132.13"
	const testName = "test-container-multiple-serving-ports-injected"
	services := map[string]apiv1.Service{
		"svc-a": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      testName + "-svc-a",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11760,
			},
		},
		"svc-b": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      testName + "-svc-b",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11761,
			},
		},
	}

	for _, svc := range services {
		t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
		err := client.Create(ctx, &svc)
		require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)
	}

	const svcAHostPort = 11770
	const svcBContainerPort = 11771
	var spAnn strings.Builder
	spAnn.WriteString("[")

	// Injected via env var, matched by host port
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s", "port":%d}`, services["svc-a"].ObjectMeta.Name, svcAHostPort))
	spAnn.WriteString(",")

	// Injected via startup parameter, matched by container port
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s","port":%d}`, services["svc-b"].ObjectMeta.Name, svcBContainerPort))

	spAnn.WriteString("]")

	const svcAContainerPort = 2345
	const imageName = testName + "-image"
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName,
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{HostPort: svcAHostPort, ContainerPort: svcAContainerPort, HostIP: IPAddr, Protocol: "tcp"},
				{ContainerPort: svcBContainerPort, HostIP: IPAddr, Protocol: "tcp"},
			},
			Env: []apiv1.EnvVar{
				{
					Name:  "SVC_A_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, services["svc-a"].ObjectMeta.Name),
				},
			},
			Args: []string{
				fmt.Sprintf(`--svc-b-port={{- portForServing "%s" -}}`, services["svc-b"].ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure that Container '%s' is started with the expected ports, env vars, and startup args...", ctr.ObjectMeta.Name)
	expectedArg := fmt.Sprintf("--svc-b-port=%d", svcBContainerPort)
	expectedEnvVar := fmt.Sprintf("SVC_A_PORT=%d", svcAContainerPort)

	_, err = ctrl_testutil.WaitForCommand(processExecutor, ctx, []string{"docker", "run"}, expectedArg, func(pe *ctrl_testutil.ProcessExecution) bool {
		if !pe.Running() {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", fmt.Sprintf("%s:%d:%d/tcp", IPAddr, svcAHostPort, svcAContainerPort)}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-p", fmt.Sprintf("%s::%d/tcp", IPAddr, svcBContainerPort)}) == -1 {
			return false
		}
		if slices.SeqIndex(pe.Cmd.Args, []string{"-e", expectedEnvVar}) == -1 {
			return false
		}
		// The expected argument is checked via "lastArg" parameter of the WaitForCommand() function above
		return true
	})
	require.NoError(t, err, "Could not find the expected 'docker run' command")

	t.Log("Complete the container startup sequence...")
	creationTime := time.Now().UTC()
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)
	err = ensureContainerRunningWithArg(t, ctx, ctr.Spec.Image, expectedArg, containerID, creationTime)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	t.Logf("Ensure the Status.EffectiveEnv for Container '%s' contains the injected ports...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		return len(currentCtr.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedCtr.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Container '%s' effective environment does not contain expected port information for service A. The effective environemtn is %v", ctr.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure the Status.EffectiveArgs for Container '%s' contains the injected port...", ctr.ObjectMeta.Name)
	require.Equal(t, updatedCtr.Status.EffectiveArgs[0], expectedArg, "The Container '%s' startup parameters do not include expected port for service B. The startup parameters are %v", ctr.ObjectMeta.Name, updatedCtr.Status.EffectiveArgs)
}

func ensureContainerRunning(t *testing.T, ctx context.Context, image string, containerID string, created time.Time) error {
	return ensureContainerRunningWithArg(t, ctx, image, image, containerID, created)
}

func ensureContainerRunningWithArg(t *testing.T, ctx context.Context, image string, lastArg string, containerID string, created time.Time) error {
	// Ensure appropriate "docker run" command has been executed
	dockerRunCmd, err := waitForDockerCommand(t, ctx, []string{"run"}, lastArg)
	if err != nil {
		return err
	}

	// Make sure we are ready for the container to be inspected
	ensureContainerInspectResponse(t, image, containerID, created, ct.ContainerStatusRunning)

	// Reply to "docker run" command with a container ID
	_, err = dockerRunCmd.Cmd.Stdout.Write([]byte(containerID + "\n"))
	if err != nil {
		t.Errorf("Could not provide container ID to the controller: %v", err)
		return err
	}
	processExecutor.SimulateProcessExit(t, dockerRunCmd.PID, 0)

	return nil
}

func simulateContainerExit(t *testing.T, ctx context.Context, image string, containerID string, created time.Time) error {
	return simulateContainerEvent(t, ctx, image, containerID, created, ct.EventActionDie, ct.ContainerStatusExited)
}

func simulateContainerStart(t *testing.T, ctx context.Context, image string, containerID string, created time.Time) error {
	return simulateContainerEvent(t, ctx, image, containerID, created, ct.EventActionStart, ct.ContainerStatusRunning)
}

func simulateContainerEvent(t *testing.T, ctx context.Context, image string, containerID string, created time.Time, event ct.EventAction, status ct.ContainerStatus) error {
	eventsCmd, err := waitForDockerCommand(t, ctx, []string{"events"}, "")
	if err != nil {
		return err
	}

	// The controller will want to inspect the exited container
	ensureContainerInspectResponse(t, image, containerID, created, status)

	// Emit container event
	_, err = eventsCmd.Cmd.Stdout.Write([]byte(getContainerEventJson(containerID, event) + "\n"))
	if err != nil {
		t.Errorf("Could not emit container Die event: %v", err)
		return err
	}

	return nil
}

func ensureContainerDeletionResponse(t *testing.T, containerID string) {
	autoExec := ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{"docker", "container", "rm"},
			LastArg: containerID,
			Cond:    (*ctrl_testutil.ProcessExecution).Running,
		},
		RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
			_, err := pe.Cmd.Stdout.Write([]byte(containerID + "\n"))
			if err != nil {
				t.Errorf("Could not simulate container removal: %v", err)
			}
			return 0
		},
	}
	processExecutor.InstallAutoExecution(autoExec)
}

func ensureContainerInspectResponse(t *testing.T, image string, containerID string, created time.Time, status ct.ContainerStatus) {
	inspectRes := getInspectedContainerJson(containerID, image, created, status) + "\n"
	autoExec := ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{"docker", "container", "inspect"},
			LastArg: containerID,
			Cond:    (*ctrl_testutil.ProcessExecution).Running,
		},
		RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
			_, err := pe.Cmd.Stdout.Write([]byte(inspectRes))
			if err != nil {
				t.Errorf("Could not provide container inspection result: %v", err)
			}
			return 0
		},
	}
	processExecutor.InstallAutoExecution(autoExec)
}

func getInspectedContainerJson(containerID string, image string, created time.Time, status ct.ContainerStatus) string {
	// Note: you can use the following command to get inspected container data from Docker CLI
	// filtered down to what container controller cares about
	// docker inspect <container id> | jq --indent 4 '.[0] | {Id, Name, Created, Config: {Image: .Config.Image}, State: {StartedAt: .State.StartedAt, Status: .State.Status, ExitCode: .State.ExitCode}}'

	started := created.Add(100 * time.Millisecond)
	retval := fmt.Sprintf(`{
		"Id": "%s",
		"Name": "/%s_name",
		"Created": "%s",
		"Config": {
			"Image": "%s"
		},
		"State": {
			"StartedAt": "%s",
			"Status": "%s",
			"ExitCode": 0
		}
	}`, containerID, containerID, created.Format(time.RFC3339), image, started.Format(time.RFC3339), status)

	return strings.ReplaceAll(retval, "\n", " ") // Make it a single-line (JSON lines) record
}

func getContainerEventJson(containerID string, action ct.EventAction) string {
	retval := fmt.Sprintf(`{
		"Type": "%s",
		"Action": "%s",
		"Actor": {
			"ID": "%s"
		} 
	}`, ct.EventSourceContainer, action, containerID)
	return strings.ReplaceAll(retval, "\n", " ")
}
