package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smallnest/chanx"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func ensureContainerRunning(t *testing.T, ctx context.Context, container *apiv1.Container) (*apiv1.Container, containers.InspectedContainer) {
	updated := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(container), func(currentContainer *apiv1.Container) (bool, error) {
		if currentContainer.Status.State == apiv1.ContainerStateFailedToStart {
			return false, fmt.Errorf("container creation failed: %s", currentContainer.Status.Message)
		}

		return currentContainer.Status.State == apiv1.ContainerStateRunning, nil
	})

	inspectedContainers, err := orchestrator.InspectContainers(ctx, []string{updated.Status.ContainerID})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspectedContainers, 1, "expected to find a single container")

	return updated, inspectedContainers[0]
}

func ensureContainerStarting(t *testing.T, ctx context.Context, container *apiv1.Container) *apiv1.Container {
	updated := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(container), func(currentContainer *apiv1.Container) (bool, error) {
		if currentContainer.Status.State == apiv1.ContainerStateFailedToStart {
			return false, fmt.Errorf("container creation failed: %s", currentContainer.Status.Message)
		}

		return currentContainer.Status.State == apiv1.ContainerStateStarting, nil
	})

	return updated
}

func TestInvalidContainerName(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-container-name",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "%%%INVALIDNAME%%%",
			Image:         "invalid-container-name-image",
		},
	}

	require.Len(t, ctr.Validate(ctx), 1, "Expected validation error for invalid container name")

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-container-name",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "a-1.2_34",
			Image:         "valid-container-name-image",
		},
	}

	require.Len(t, ctr.Validate(ctx), 0, "Unexpected validation error for valid container name")
}

// Ensure a container instance is started when new Container object appears
func TestContainerInstanceStarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-instance-starts"
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

	_, _ = ensureContainerRunning(t, ctx, &ctr)
}

func TestContainerMarkedDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-marked-done"
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

	updatedContainer, _ := ensureContainerRunning(t, ctx, &ctr)

	err = orchestrator.SimulateContainerExit(ctx, updatedContainer.Status.ContainerID)
	require.NoError(t, err, "could not simulate container exit")

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

	// Cause the orchestrator to fail to create the container
	errMsg := fmt.Sprintf("Container '%s' could not be run because it is just a test :-)", testName)
	orchestrator.FailMatchingContainers(ctx, testName, 1, errMsg)

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	// The container should be marked as "failed to start", and the Message property of its status
	// should contain the error from container orchestrator, i.e. Docker
	t.Log("Ensure container state is 'failed to start'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateFailedToStart
		messageOK := strings.Contains(c.Status.Message, errMsg)
		return statusUpdated && messageOK, nil
	})
}

func validatePorts(t *testing.T, inspected containers.InspectedContainer, ports []apiv1.ContainerPort) {
	for _, port := range ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		mappings, found := inspected.Ports[fmt.Sprintf("%d/%s", port.ContainerPort, protocol)]
		require.True(t, found, "container port %d/%s was not published", port.ContainerPort, port.Protocol)
		require.Len(t, mappings, 1, "expected a single mapping for container port %d", port.ContainerPort)

		hostPort := port.HostPort
		if hostPort == 0 {
			hostPort = port.ContainerPort
		}
		require.Equal(t, fmt.Sprintf("%d", hostPort), mappings[0].HostPort, "expected the host port to be %s", hostPort)

		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
		require.Equal(t, hostIP, mappings[0].HostIp, "expected the host IP to be %s", hostIP)
	}

	require.Len(t, maps.Keys(inspected.Ports), len(ports), "did not find expected number of ports")
}

// If ports are part of the spec, they are published to the host
func TestContainerStartWithPorts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// Case 1: just ContainerPort
	testName := "container-start-with-ports-case1"
	imageName := testName + "-image"

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

	_, inspected := ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 2: ContainerPort and HostPort
	testName = "container-start-with-ports-case2"
	imageName = testName + "-image"

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

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 3: ContainerPort and HostIP
	testName = "container-start-with-ports-case3"
	imageName = testName + "-image"

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

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 4: ContainerPort, HostIP, and Portocol
	testName = "container-start-with-ports-case4"
	imageName = testName + "-image"

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

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 5: ContainerPort, HostIP, HostPort, and Protocol
	testName = "container-start-with-ports-case5"
	imageName = testName + "-image"

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

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)
}

func TestContainerStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-stop-state"
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

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Stopping Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Stop = true
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Log("Ensure container state is 'Exited'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})

	inspected, err := orchestrator.InspectContainers(ctx, []string{updatedCtr.Status.ContainerID})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", ctr.ObjectMeta.Name)
	waitObjectDeleted[apiv1.Container](t, ctx, ctrl_client.ObjectKeyFromObject(&ctr))

	inspected, err = orchestrator.InspectContainers(ctx, []string{updatedCtr.Status.ContainerID})
	require.Error(t, err, "expected the container to be gone")
	require.Len(t, inspected, 0, "expected the container to be gone")
}

func TestContainerDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-deletion"
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

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	// Subscribe to container events to verify that the container is deleted gracefully
	ctrEventCh := chanx.NewUnboundedChan[containers.EventMessage](ctx, 2)
	ctrEventSub, watchErr := orchestrator.WatchContainers(ctrEventCh.In)
	require.NoError(t, watchErr, "could not subscribe to container events")
	defer func() {
		cancelErr := ctrEventSub.Cancel()
		require.NoError(t, cancelErr, "could not cancel the container event subscription")
	}()

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	waitObjectDeleted[apiv1.Container](t, ctx, ctrl_client.ObjectKeyFromObject(&ctr))

	t.Logf("Ensure that the Container '%s' is stopped and removed gracefully...", ctr.ObjectMeta.Name)
	stopCount := 0
	removeCount := 0

readEvents:
	for {
		select {
		case event, isOpen := <-ctrEventCh.Out:
			if !isOpen {
				t.Fatal("container event channel was closed unexpectedly")
			}

			if event.Actor.ID != updatedCtr.Status.ContainerID {
				break
			}

			// Note: containers.EventActionStop is raised when the container is killed,
			// so it is not a good indicator that the container is being stopped gracefully.

			switch event.Action {
			case ctrl_testutil.TestEventActionStopWithoutRemove:
				stopCount++
			case containers.EventActionDestroy:
				removeCount++
			}

			if stopCount == 1 && removeCount == 1 {
				t.Logf("Container '%s' was stopped and removed gracefully", ctr.ObjectMeta.Name)
				break readEvents
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for container events")
		}
	}
}

func TestContainerRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// If a container shuts down and then restarts, it should be tracked as running
	const testName = "container-restart"
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
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)
	err = orchestrator.SimulateContainerExit(ctx, updatedCtr.Status.ContainerID)
	require.NoError(t, err, "could not simulate container exit")

	t.Log("Ensure container state is 'stopped'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateExited
		return statusUpdated, nil
	})

	_, err = orchestrator.StartContainers(ctx, []string{updatedCtr.Status.ContainerID})
	require.NoError(t, err, "could not simulate container start")

	t.Log("Ensure container state is 'running'...")
	_, _ = ensureContainerRunning(t, ctx, &ctr)
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

	_, inspected := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Ensure that Container '%s' is started with the expected ports, env vars, and startup args...", ctr.ObjectMeta.Name)
	expectedArg := fmt.Sprintf("--svc-b-port=%d", svcBContainerPort)
	require.Contains(t, inspected.Args, expectedArg, "expected the container to have the startup arg %s", expectedArg)

	expectedEnvVar := fmt.Sprintf("SVC_A_PORT=%d", svcAContainerPort)
	require.Equal(t, fmt.Sprintf("%d", svcAContainerPort), inspected.Env["SVC_A_PORT"], "expected the container to have the env var %s", expectedEnvVar)

	validatePorts(t, inspected, []apiv1.ContainerPort{
		{ContainerPort: svcAContainerPort, HostPort: svcAHostPort, HostIP: IPAddr, Protocol: "tcp"},
		{ContainerPort: svcBContainerPort, HostIP: IPAddr, Protocol: "tcp"},
	})

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

	t.Logf("Ensure services exposed by Container '%s' get to Ready state...", ctr.ObjectMeta.Name)
	for _, svc := range services {
		waitServiceReady(t, ctx, &svc)
	}
}

func TestContainerServingAddressInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-serving-address-injected"
	const ServiceIPAddr = "127.63.29.2"
	const ContainerIPAddr = "127.63.29.3"

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  ServiceIPAddr,
			Port:     26003,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)

	const ContainerPort = 26004
	var spAnn strings.Builder
	spAnn.WriteString("[")
	spAnn.WriteString(fmt.Sprintf(`{ "serviceName":"%s", "address": "%s", "port": %d }`, svc.ObjectMeta.Name, ContainerIPAddr, ContainerPort))
	spAnn.WriteString("]")

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName + "-server",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ContainerSpec{
			Image: testName + "-image",
			Ports: []apiv1.ContainerPort{
				{ContainerPort: ContainerPort},
			},
			Env: []apiv1.EnvVar{
				{
					Name:  "SERVICE_ADDRESS",
					Value: fmt.Sprintf(`{{- addressFor "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
			Args: []string{
				fmt.Sprintf(`--serving-address={{- addressForServing "%s" -}}`, svc.ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure that Container '%s' is started with the expected ports, env vars, and startup args...", ctr.ObjectMeta.Name)
	expectedArg := fmt.Sprintf("--serving-address=%s", ContainerIPAddr)
	expectedEnvVar := fmt.Sprintf("SERVICE_ADDRESS=%s", ServiceIPAddr)

	_, inspected := ensureContainerRunning(t, ctx, &ctr)
	require.Contains(t, inspected.Args, expectedArg, "expected the container to have the startup arg %s", expectedArg)
	require.Equal(t, ServiceIPAddr, inspected.Env["SERVICE_ADDRESS"], "expected the container to have the env var %s", expectedEnvVar)
	validatePorts(t, inspected, []apiv1.ContainerPort{
		{ContainerPort: ContainerPort, HostIP: "127.0.0.1"},
	})

	t.Logf("Ensure the Status.EffectiveEnv for Container '%s' contains the injected address information...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		return len(currentCtr.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedCtr.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Container '%s' effective environment does not contain expected address information for service '%s'. The effective environemtn is %v", ctr.ObjectMeta.Name, svc.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure the Status.EffectiveArgs for Container '%s' contains the injected address information...", ctr.ObjectMeta.Name)
	require.Equal(t, updatedCtr.Status.EffectiveArgs[0], expectedArg, "The Container '%s' startup parameters do not include expected address information for service '%s'. The startup parameters are %v", ctr.ObjectMeta.Name, svc.ObjectMeta.Name, updatedCtr.Status.EffectiveArgs)

	t.Logf("Ensure service exposed by Container '%s' gets to Ready state...", ctr.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

func TestPersistentContainerDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-deletion"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	waitObjectDeleted[apiv1.Container](t, ctx, ctrl_client.ObjectKeyFromObject(&ctr))

	inspected, err := orchestrator.InspectContainers(ctx, []string{updatedCtr.Status.ContainerID})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
}

func TestPersistentContainerAlreadyExists(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-already-exists"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
		},
	}

	id, err := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: ctr.Spec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = orchestrator.StartContainers(ctx, []string{id})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	require.Equal(t, id, updatedCtr.Status.ContainerID, "container ID does not match existing value")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	waitObjectDeleted[apiv1.Container](t, ctx, ctrl_client.ObjectKeyFromObject(&ctr))

	inspected, err := orchestrator.InspectContainers(ctx, []string{updatedCtr.Status.ContainerID})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
}
