package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestProxyStartedAndStopped(t *testing.T) {
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

	t.Log("Check if service status updated...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		proxyPidPresent := s.Status.ProxyProcessPid != 0
		addressCorrect := s.Status.EffectiveAddress == svc.Spec.Address
		portCorrect := s.Status.EffectivePort == svc.Spec.Port
		return proxyPidPresent && addressCorrect && portCorrect, nil
	})

	selector := func(pe ctrl_testutil.ProcessExecution) bool {
		hasAddressCanary := slices.Any(pe.Cmd.Args, func(arg string) bool {
			return strings.Contains(arg, proxyAddress)
		})
		hasPortCanary := slices.Any(pe.Cmd.Args, func(arg string) bool {
			return strings.Contains(arg, fmt.Sprintf("%d", proxyPort))
		})

		return hasAddressCanary && hasPortCanary
	}

	t.Log("Check if corresponding proxy process has started...")
	err = ensureProxyProcess(ctx, selector)
	require.NoError(t, err, "Could not ensure proxy process running")

	t.Log("Delete service...")
	err = client.Delete(ctx, &svc)
	require.NoError(t, err, "Could not delete a Service")

	t.Log("Check if service deleted...")
	waitObjectDeleted[apiv1.Service](t, ctx, ctrl_client.ObjectKeyFromObject(&svc))
	t.Log("Service deleted.")

	t.Log("Check if corresponding proxy process has stopped...")
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

func TestRandomPort(t *testing.T) {
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
		addressCorrect := s.Status.EffectiveAddress == "127.0.0.1" // The default address for default AddressAllocationMode
		portCorrect := s.Status.EffectivePort > 0
		return addressCorrect && portCorrect, nil
	})
	t.Log("Service has random port.")
}

func TestIPv6Address(t *testing.T) {
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

func ensureProxyProcess(ctx context.Context, selector func(pe ctrl_testutil.ProcessExecution) bool) error {
	processStarted := func(_ context.Context) (bool, error) {
		runningProcessesWithPath := processExecutor.FindAll("traefik", selector)

		if len(runningProcessesWithPath) != 1 {
			return false, nil
		}

		return true, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, processStarted)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func ensureProxyProcessStopped(ctx context.Context, selector func(pe ctrl_testutil.ProcessExecution) bool) error {
	return ensureProxyProcess(ctx, func(pe ctrl_testutil.ProcessExecution) bool {
		return selector(pe) && !pe.EndedAt.IsZero() && pe.ExitCode == ctrl_testutil.KilledProcessExitCode
	})
}
