package integration_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/nettest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

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

// Ensure that a service implemented by ExecutableReplicaSet becomes Ready.
func TestServicesBecomeReadyMultipleReplicas(t *testing.T) {
	type testcase struct {
		description string
		svc         *apiv1.Service
		ers         *apiv1.ExecutableReplicaSet
	}

	testcases := []testcase{
		{
			description: "Service backed by multiple replicas started by process runner",
			svc: &apiv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc-becomes-ready-multi-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ServiceSpec{
					Protocol: apiv1.TCP,
					Address:  "127.32.15.120",
					Port:     19825,
				},
			},
			ers: &apiv1.ExecutableReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ers-becomes-ready-multi-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableReplicaSetSpec{
					Replicas: 3,
					Template: apiv1.ExecutableTemplate{
						Annotations: map[string]string{
							"service-producer": fmt.Sprintf(`[{"serviceName":"%s"}]`, "svc-becomes-ready-multi-process"),
						},
						Spec: apiv1.ExecutableSpec{
							ExecutablePath: "path/to/ers-becomes-ready-multi-process",
							Env: []apiv1.EnvVar{
								{
									Name:  "PORT",
									Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, "svc-becomes-ready-multi-process"),
								},
							},
						},
					},
				},
			},
		},
		{
			description: "Service backed by multiple replicas started by IDE runner",
			svc: &apiv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc-becomes-ready-multi-ide",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ServiceSpec{
					Protocol: apiv1.TCP,
					Address:  "127.32.15.121",
					Port:     19826,
				},
			},
			ers: &apiv1.ExecutableReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ers-becomes-ready-multi-ide",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableReplicaSetSpec{
					Replicas: 3,
					Template: apiv1.ExecutableTemplate{
						Annotations: map[string]string{
							"service-producer":                          fmt.Sprintf(`[{"serviceName":"%s"}]`, "svc-becomes-ready-multi-ide"),
							ctrl_testutil.AutoStartExecutableAnnotation: "true",
						},
						Spec: apiv1.ExecutableSpec{
							ExecutablePath: "path/to/ers-becomes-ready-multi-ide",
							Env: []apiv1.EnvVar{
								{
									Name:  "PORT",
									Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, "svc-becomes-ready-multi-ide"),
								},
							},
							ExecutionType: apiv1.ExecutionTypeIDE,
						},
					},
				},
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			t.Logf("Creating Service '%s'", tc.svc.ObjectMeta.Name)
			err := client.Create(ctx, tc.svc)
			require.NoError(t, err, "Could not create Service %s", tc.svc.ObjectMeta.Name)

			t.Logf("Creating ExecutableReplicaSet '%s'", tc.ers.ObjectMeta.Name)
			err = client.Create(ctx, tc.ers)
			require.NoError(t, err, "Could not create ExecutableReplicaSet %s", tc.ers.ObjectMeta.Name)

			t.Logf("Ensure all replicas for ExecutableReplicaSet '%s' are running...", tc.ers.ObjectMeta.Name)
			waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.ers), func(ers *apiv1.ExecutableReplicaSet) (bool, error) {
				return ers.Status.RunningReplicas == ers.Spec.Replicas, nil
			})

			t.Logf("Ensure Service '%s' is in Ready state...", tc.svc.ObjectMeta.Name)
			waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.svc), func(svc *apiv1.Service) (bool, error) {
				return svc.Status.State == apiv1.ServiceStateReady, nil
			})
		})
	}
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
	_, _ = ensureContainerRunning(t, ctx, &container)
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

// Tests that port injections via environment variables works even if the Service does not have the port allocated initially.
// Eventually we want service consumers to be able to start up even if the Service does not exist at all
// (https://github.com/microsoft/usvc/issues/111), but for now this is as far as we go.
func TestServiceConsumableAfterLatePortAllocation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-service-consumable-after-late-port-allocation"
	const svcAddress = "127.0.0.1"
	const endpointPort = 49902

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-service",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create Service '%s", svc.ObjectMeta.Name)

	t.Logf("Verify Service '%s' is in NotReady state...", svc.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		notReady := s.Status.State == apiv1.ServiceStateNotReady
		noEffectivePort := s.Status.EffectivePort == 0
		return notReady && noEffectivePort, nil
	})

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-exe",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + testName + "-exe",
			Env: []apiv1.EnvVar{
				{
					Name:  "PORT",
					Value: fmt.Sprintf(`{{- portFor "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create Executable '%s", exe.ObjectMeta.Name)

	const imageName = testName + "-image"
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-ctr",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Env: []apiv1.EnvVar{
				{
					Name:  "PORT",
					Value: fmt.Sprintf(`{{- portFor "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	// Wait a bit to give Executable and Container controllers a chance to attempt starting their objects.
	// Both attempts will fail because the service does not have a port allocated yet.
	time.Sleep(1 * time.Second)

	end := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-endpoint",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.ObjectMeta.Namespace,
			ServiceName:      svc.ObjectMeta.Name,
			Address:          svcAddress,
			Port:             endpointPort,
		},
	}

	t.Logf("Creating Endpoint '%s'", end.ObjectMeta.Name)
	err = client.Create(ctx, &end)
	require.NoError(t, err, "Could not create Endpoint '%s", end.ObjectMeta.Name)

	t.Logf("Ensure Service '%s' assumed Ready state...", svc.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		correctState := s.Status.State == apiv1.ServiceStateReady
		portCorrect := s.Status.EffectivePort == endpointPort
		return correctState && portCorrect, nil
	})

	t.Logf("Complete the Container '%s' startup sequence...", ctr.ObjectMeta.Name)
	_, _ = ensureContainerRunning(t, ctx, &ctr)
	require.NoError(t, err, "Container '%s' was not started as expected", ctr.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' is running and has the Service port injected...", exe.ObjectMeta.Name)
	expectedEnvVar := fmt.Sprintf("PORT=%d", endpointPort)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		effectiveEnv := slices.Map[apiv1.EnvVar, string](currentExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
			return fmt.Sprintf("%s=%s", v.Name, v.Value)
		})
		running := currentExe.Status.State == apiv1.ExecutableStateRunning
		hasEnvVar := slices.Contains(effectiveEnv, expectedEnvVar)
		return running && hasEnvVar, nil
	})

	t.Logf("Ensure Container '%s' is running and has the Service port injected...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		effectiveEnv := slices.Map[apiv1.EnvVar, string](currentCtr.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
			return fmt.Sprintf("%s=%s", v.Name, v.Value)
		})
		running := currentCtr.Status.State == apiv1.ContainerStateRunning
		hasEnvVar := slices.Contains(effectiveEnv, expectedEnvVar)
		return running && hasEnvVar, nil
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
		if s.Status.EffectiveAddress == "" || s.Status.EffectivePort == 0 {
			return false, nil
		}
		_, addressResolutionErr := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", s.Status.EffectiveAddress, s.Status.EffectivePort))
		return addressResolutionErr == nil, nil
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

	if !nettest.SupportsIPv6() {
		return
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
	_, _ = ensureContainerRunning(t, ctx, &container)
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
	err = retryOnConflict(ctx, endpoint1.NamespacedName(), func(ctx context.Context, currentEndpoint *apiv1.Endpoint) error {
		return client.Delete(ctx, currentEndpoint)
	})
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
	err = retryOnConflict(ctx, endpoint2.NamespacedName(), func(ctx context.Context, currentEndpoint *apiv1.Endpoint) error {
		return client.Delete(ctx, currentEndpoint)
	})
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
