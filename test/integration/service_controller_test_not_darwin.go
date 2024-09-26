//go:build !darwin

package integration_test

import (
	"testing"

	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

func TestServiceRandomIPv4Address(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-randomipv4",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeIPv4Loopback,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create a Service")

	t.Log("Check if Service has random IPv4 address...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&svc), func(s *apiv1.Service) (bool, error) {
		addressCorrect := s.Status.EffectiveAddress != networking.Ipv4LocalhostDefaultAddress && s.Status.EffectiveAddress != ""
		portCorrect := s.Status.EffectivePort > 0
		return addressCorrect && portCorrect, nil
	})
	t.Log("Service has random IPv4 address.")
}
