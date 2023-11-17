package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestServiceProxyStartedAndStopped(t *testing.T) {
	proxyAddress := "127.1.2.3"
	proxyPort := int32(1234)

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxy-started",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  proxyAddress,
			Port:     proxyPort,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service")

	t.Logf("Check if Service '%s' status was updated...", svc.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		proxyPidPresent := s.Status.ProxyProcessPid != apiv1.UnknownPID
		addressCorrect := s.Status.EffectiveAddress == svc.Spec.Address
		portCorrect := s.Status.EffectivePort == svc.Spec.Port
		return proxyPidPresent && addressCorrect && portCorrect, nil
	})

	selector := func(pe *ctrl_testutil.ProcessExecution) bool {
		hasAddressCanary := slices.Any(pe.Cmd.Args, func(arg string) bool {
			return strings.Contains(arg, proxyAddress)
		})
		hasPortCanary := slices.Any(pe.Cmd.Args, func(arg string) bool {
			return strings.Contains(arg, fmt.Sprintf("%d", proxyPort))
		})

		return hasAddressCanary && hasPortCanary
	}

	t.Log("Check if corresponding proxy process has started...")
	proxyProcess, err := ensureProxyProcess(ctx, selector)
	require.NoError(t, err, "Could not ensure proxy process running")

	t.Log("Killing proxy process to ensure it is restarted upon crash...")
	processExecutor.SimulateProcessExit(t, proxyProcess.PID, 1)

	selector2 := func(pe *ctrl_testutil.ProcessExecution) bool {
		return selector(pe) && pe.PID != proxyProcess.PID
	}

	t.Log("Check if corresponding proxy process has restarted...")
	proxyProcess2, err := ensureProxyProcess(ctx, selector2)
	require.NoError(t, err, "Could not ensure proxy process running")
	require.True(t, proxyProcess2.Running(), "Proxy process is not running")

	t.Log("Delete service...")
	err = client.Delete(ctx, &svc)
	require.NoError(t, err, "Could not delete a Service")

	t.Logf("Check if Service '%s' was deleted...", svc.ObjectMeta.Name)
	waitObjectDeleted[apiv1.Service](t, ctx, ctrl_client.ObjectKeyFromObject(&svc))
	t.Log("Service deleted.")

	t.Logf("Check if proxy process for Service '%s' has stopped...", svc.ObjectMeta.Name)
	err = ensureProxyProcessStopped(ctx, selector)
	require.NoError(t, err, "Could not ensure proxy process stopped")
	t.Log("Proxy process has stopped.")
}

func TestServiceBecomesReady(t *testing.T) {
	proxyAddress := "127.5.6.7"
	proxyPort := int32(5678)

	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-ready",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  proxyAddress,
			Port:     proxyPort,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service")

	t.Log("Check if Service state NotReady...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		correctState := s.Status.State == apiv1.ServiceStateNotReady
		addressCorrect := s.Status.EffectiveAddress == svc.Spec.Address
		portCorrect := s.Status.EffectivePort == svc.Spec.Port
		return correctState && addressCorrect && portCorrect, nil
	})
	t.Log("Service is in state NotReady.")

	end := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-endpoint-ready",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.ObjectMeta.Namespace,
			ServiceName:      svc.ObjectMeta.Name,
			Address:          "127.0.0.1",
			Port:             1234,
		},
	}

	t.Logf("Creating Endpoint '%s'", end.ObjectMeta.Name)
	err = client.Create(ctx, &end)
	require.NoError(t, err, "Could not create an Endpoint")

	t.Log("Check if Service state Ready...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		correctState := s.Status.State == apiv1.ServiceStateReady
		addressCorrect := s.Status.EffectiveAddress == svc.Spec.Address
		portCorrect := s.Status.EffectivePort == svc.Spec.Port
		return correctState && addressCorrect && portCorrect, nil
	})
	t.Log("Service is in state Ready.")
}

// Tests that service starts proxying and becomes ready when it is created AFTER the Executable and Container
// serving the service have been created.
func TestServiceDelayedCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svcName := "test-service-delayed-creation"

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-service-delayed-creation",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":56789}]`, svcName)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-service-delayed-creation",
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svcName)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	_, err = ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process could not be started")

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        svcName + "-container",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","port":80}]`, svcName)},
		},
		Spec: apiv1.ContainerSpec{
			Image: svcName + "-image",
			Ports: []apiv1.ContainerPort{
				{
					ContainerPort: 80,
					HostPort:      56790,
				},
			},
		},
	}

	t.Logf("Creating Container '%s' that is producing the Service '%s'...", container.ObjectMeta.Name, svcName)
	err = client.Create(ctx, &container)
	require.NoError(t, err, "Could not create the Container")

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	containerID := container.ObjectMeta.Name + "-" + testutil.GetRandLetters(t, 6)
	err = ensureContainerRunning(t, ctx, container.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.10.10.134",
			Port:     57003,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err = client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	// Because the Executable and Container above were created before the Service,
	// they should also have corresponding Endpoints created for them,
	// and the Service should be able to assume Ready state almost immediately.
	t.Log("Check if Service is in Ready state...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		correctState := s.Status.State == apiv1.ServiceStateReady
		addressCorrect := s.Status.EffectiveAddress == svc.Spec.Address
		portCorrect := s.Status.EffectivePort == svc.Spec.Port
		return correctState && addressCorrect && portCorrect, nil
	})
}

func TestServiceRandomPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-randomport",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service")

	t.Log("Check if Service has random port...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == "localhost" // The default address for default AddressAllocationMode
		portCorrect := s.Status.EffectivePort > 0
		return addressCorrect && portCorrect, nil
	})
	t.Log("Service has random port.")
}

func TestServiceIPv6Address(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-ipv6",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeIPv6ZeroOne,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service")

	t.Log("Check if Service has IPv6 address...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == "[::1]"
		portCorrect := s.Status.EffectivePort > 0
		return addressCorrect && portCorrect, nil
	})
	t.Log("Service has IPv6 address.")
}

func TestServiceProxyless(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svcName := "test-service-proxyless"
	containerID := svcName + "-" + testutil.GetRandLetters(t, 6)

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        svcName + "-container",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","port":80}]`, svcName)},
		},
		Spec: apiv1.ContainerSpec{
			Image: svcName + "-image",
			Ports: []apiv1.ContainerPort{
				{
					ContainerPort: 80,
					HostPort:      56791,
				},
			},
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service %s", svc.ObjectMeta.Name)

	t.Logf("Creating Container '%s' that is producing the Service '%s'...", container.ObjectMeta.Name, svcName)
	err = client.Create(ctx, &container)
	require.NoError(t, err, "Could not create the Container %s", container.ObjectMeta.Name)

	t.Logf("Check if corresponding container %s has started...", container.ObjectMeta.Name)
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, container.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container %s was not started as expected", container.ObjectMeta.Name)

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == "127.0.0.1" // The default address for Proxyless containers
		portCorrect := s.Status.EffectivePort == 56791
		return addressCorrect && portCorrect, nil
	})
}

func TestServiceProxylessWithMultipleEndpoints(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svcName := "test-service-proxyless-multiple-endpoints"

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	endpoint1 := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName + "-endpoint1",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.ObjectMeta.Namespace,
			ServiceName:      svc.ObjectMeta.Name,
			Address:          "localhost",
			Port:             56792,
		},
	}

	endpoint2 := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName + "-endpoint2",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.ObjectMeta.Namespace,
			ServiceName:      svc.ObjectMeta.Name,
			Address:          "localhost",
			Port:             56793,
		},
	}

	endpoint3 := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName + "-endpoint3",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.ObjectMeta.Namespace,
			ServiceName:      svc.ObjectMeta.Name,
			Address:          "localhost",
			Port:             56794,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service %s", svc.ObjectMeta.Name)

	// Ensure Service is not ready
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		return s.Status.State == apiv1.ServiceStateNotReady, nil
	})

	// Create endpoint1
	t.Logf("Creating Endpoint '%s'", endpoint1.ObjectMeta.Name)
	err = client.Create(ctx, &endpoint1)
	require.NoError(t, err, "Could not create an Endpoint %s", endpoint1.ObjectMeta.Name)

	// Ensure Service switches to endpoint1
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == endpoint1.Spec.Address
		portCorrect := s.Status.EffectivePort == endpoint1.Spec.Port
		endpointNamespaceCorrect := s.Status.ProxylessEndpointNamespace == endpoint1.ObjectMeta.Namespace
		endpointNameCorrect := s.Status.ProxylessEndpointName == endpoint1.ObjectMeta.Name
		return addressCorrect && portCorrect && endpointNamespaceCorrect && endpointNameCorrect, nil
	})

	// Create endpoint2
	t.Logf("Creating Endpoint '%s'", endpoint2.ObjectMeta.Name)
	err = client.Create(ctx, &endpoint2)
	require.NoError(t, err, "Could not create an Endpoint %s", endpoint2.ObjectMeta.Name)

	// Wait a second
	time.Sleep(1 * time.Second)

	// Ensure Service has stayed on endpoint1
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == endpoint1.Spec.Address
		portCorrect := s.Status.EffectivePort == endpoint1.Spec.Port
		endpointNamespaceCorrect := s.Status.ProxylessEndpointNamespace == endpoint1.ObjectMeta.Namespace
		endpointNameCorrect := s.Status.ProxylessEndpointName == endpoint1.ObjectMeta.Name
		return addressCorrect && portCorrect && endpointNamespaceCorrect && endpointNameCorrect, nil
	})

	// Delete endpoint1
	t.Logf("Deleting Endpoint '%s'", endpoint1.ObjectMeta.Name)
	err = client.Delete(ctx, &endpoint1)
	require.NoError(t, err, "Could not delete an Endpoint %s", endpoint1.ObjectMeta.Name)

	// Ensure Service switches to endpoint2
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == endpoint2.Spec.Address
		portCorrect := s.Status.EffectivePort == endpoint2.Spec.Port
		endpointNamespaceCorrect := s.Status.ProxylessEndpointNamespace == endpoint2.ObjectMeta.Namespace
		endpointNameCorrect := s.Status.ProxylessEndpointName == endpoint2.ObjectMeta.Name
		return addressCorrect && portCorrect && endpointNamespaceCorrect && endpointNameCorrect, nil
	})

	// Delete endpoint2
	t.Logf("Deleting Endpoint '%s'", endpoint2.ObjectMeta.Name)
	err = client.Delete(ctx, &endpoint2)
	require.NoError(t, err, "Could not delete an Endpoint %s", endpoint2.ObjectMeta.Name)

	// Ensure Service is no longer ready
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		return s.Status.State == apiv1.ServiceStateNotReady, nil
	})

	// Create endpoint3
	t.Logf("Creating Endpoint '%s'", endpoint3.ObjectMeta.Name)
	err = client.Create(ctx, &endpoint3)
	require.NoError(t, err, "Could not create an Endpoint %s", endpoint3.ObjectMeta.Name)

	// Ensure Service switches to endpoint3
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress == endpoint3.Spec.Address
		portCorrect := s.Status.EffectivePort == endpoint3.Spec.Port
		endpointNamespaceCorrect := s.Status.ProxylessEndpointNamespace == endpoint3.ObjectMeta.Namespace
		endpointNameCorrect := s.Status.ProxylessEndpointName == endpoint3.ObjectMeta.Name
		return addressCorrect && portCorrect && endpointNamespaceCorrect && endpointNameCorrect, nil
	})
}

func ensureProxyProcess(ctx context.Context, selector func(pe *ctrl_testutil.ProcessExecution) bool) (*ctrl_testutil.ProcessExecution, error) {
	var processExecution *ctrl_testutil.ProcessExecution

	processStarted := func(_ context.Context) (bool, error) {
		processesWithPath := processExecutor.FindAll([]string{"traefik"}, "", selector)

		if len(processesWithPath) != 1 {
			return false, nil
		} else {
			processExecution = &processesWithPath[0]
			return true, nil
		}
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, processStarted)
	if err != nil {
		return nil, err
	} else {
		return processExecution, nil
	}
}

func ensureProxyProcessStopped(ctx context.Context, selector func(pe *ctrl_testutil.ProcessExecution) bool) error {
	_, err := ensureProxyProcess(ctx, func(pe *ctrl_testutil.ProcessExecution) bool {
		return selector(pe) && pe.Finished() && pe.ExitCode == ctrl_testutil.KilledProcessExitCode
	})

	return err
}
