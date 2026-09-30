/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

// Verifies that network creation resolves the native name response through inspection and returns the full network ID.
func TestCreateNetworkReturnsInspectedFullID(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "create", "--label", "owner=dcp", "network-name"},
		"network-name\n",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Id":"full-network-id","Name":"network-name","Driver":"bridge","Scope":"local","IPAM":{"Config":[]},"Labels":{"owner":"dcp"},"Containers":{}}]`,
		"",
		0,
	)

	networkID, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name:   "network-name",
		Labels: map[string]string{"owner": "dcp"},
	})

	require.NoError(t, createErr)
	require.Equal(t, "full-network-id", networkID)
}

// Verifies that unsupported IPv6 enablement returns an error without issuing a native network-create command.
func TestCreateNetworkRejectsIPv6WithoutInvokingCli(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	_, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "network-name",
		IPv6: true,
	})

	require.ErrorContains(t, createErr, "does not support enabling IPv6")
	require.Empty(t, executor.Executions)
}

// Verifies normalization of native network identity, IPv6/attachment flags, subnets, gateways, and connected-container addresses.
func TestInspectNetworksMapsDockerLikeWslcShape(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{
			"Id":"network-id",
			"Name":"network-name",
			"Created":"2026-01-02T03:04:05Z",
			"Scope":"local",
			"Driver":"bridge",
			"EnableIPv6":true,
			"Internal":false,
			"Attachable":true,
			"Ingress":false,
			"IPAM":{"Config":[{"Subnet":"172.20.0.0/16","Gateway":"172.20.0.1"}]},
			"Labels":{"owner":"dcp"},
			"Containers":{"container-id":{"Name":"container-name"}}
		}]`,
		"",
		0,
	)

	inspected, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{"network-name"},
	})

	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Equal(t, "network-id", inspected[0].Id)
	require.Equal(t, "network-name", inspected[0].Name)
	require.True(t, inspected[0].IPv6)
	require.True(t, inspected[0].Attachable)
	require.Equal(t, []string{"172.20.0.0/16"}, inspected[0].Subnets)
	require.Equal(t, []string{"172.20.0.1"}, inspected[0].Gateways)
	require.Equal(t, containers.InspectedNetworkContainer{
		Id:   "container-id",
		Name: "container-name",
	}, inspected[0].Containers[0])
}

// Verifies that network inspection retains a valid network while reporting missing references and incomplete results.
func TestInspectNetworksPreservesValidObjectAlongsideMissingReference(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "present", "missing"},
		`[{"Id":"network-id","Name":"present","Driver":"bridge","IPAM":{"Config":[]},"Containers":{}}]`,
		"Network not found: 'missing'\n",
		1,
	)

	inspected, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{"present", "missing"},
	})

	require.Len(t, inspected, 1)
	require.Equal(t, "network-id", inspected[0].Id)
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.ErrorIs(t, inspectErr, containers.ErrIncomplete)
}

// Verifies that network removal resolves IDs to native names but returns the identifiers originally requested by the caller.
func TestRemoveNetworkResolvesIDToNativeNameAndReturnsRequestedID(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "remove", "--force", "network-name"},
		"network-name\n",
		"",
		0,
	)

	removed, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{"network-id"},
		Force:    true,
	})

	require.NoError(t, removeErr)
	require.Equal(t, []string{"network-id"}, removed)
	require.Empty(t, executor.FindAll([]string{"wslc", "network", "remove", "--force", "network-id"}, "", nil))
}

// Verifies that network connection uses the resolved native name and preserves requested aliases in WSLC syntax.
func TestConnectNetworkUsesResolvedNameAndNativeAliasFlag(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "connect", "--network-alias", "one", "--network-alias", "two", "network-name", "container"},
		"",
		"",
		0,
	)

	connectErr := orchestrator.ConnectNetwork(ctx, containers.ConnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Aliases:   []string{"one", "two"},
	})

	require.NoError(t, connectErr)
}

// Verifies that force-disconnect accepts an ordinary-command failure only after detachment is verified, without sending an unsupported force flag.
func TestForcedDisconnectAcceptsAlreadyDetachedContainerAfterVerification(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"container is not connected to network\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","State":{"Status":"running"},"NetworkSettings":{"Networks":{}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.NoError(t, disconnectErr)
	require.Empty(t, executor.FindAll([]string{"wslc", "network", "disconnect", "--force"}, "", nil))
}

// Verifies that an apparently detached container returned alongside an inspection error does not establish successful forced disconnection.
func TestForcedDisconnectRejectsDetachedContainerReturnedWithInspectError(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"disconnect failed\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","NetworkSettings":{"Networks":{}}}]`,
		"container inspection failed\n",
		1,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, "disconnect failed")
	require.ErrorContains(t, disconnectErr, "returned an object with an error")
	require.ErrorContains(t, disconnectErr, "container inspection failed")
}

// Verifies that forced disconnection checks a stopped container's configured networks rather than relying only on active network endpoints.
func TestForcedDisconnectVerifiesExitedContainerConfiguration(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container-id"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container-id"},
		`[{"Id":"container-id","Name":"/container-name","State":{"Status":"exited"},"NetworkSettings":{"Networks":{}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container-id",
		Force:     true,
	})

	require.NoError(t, disconnectErr)
}

// Verifies that post-disconnect inspection lacking a network ID cannot establish detachment from the intended network.
func TestForcedDisconnectRejectsPostNetworkWithoutID(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","NetworkSettings":{"Networks":{}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, "inspection returned an empty ID")
}

// Verifies that a same-named replacement network with a different ID is not accepted as verification of the original disconnection.
func TestForcedDisconnectRejectsChangedNetworkIdentity(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","NetworkSettings":{"Networks":{}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Id":"replacement-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, `identity changed from "network-id" to "replacement-id"`)
}

// Verifies that forced disconnection fails when the target network remains in the container configuration.
func TestForcedDisconnectReportsIncompleteContainerConfiguration(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","NetworkSettings":{"Networks":{"network-name":{}}}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, "remains in container")
}

// Verifies that empty container-inspection output without a not-found error is not treated as confirmed detachment.
func TestForcedDisconnectDoesNotTreatEmptyContainerInspectionAsVerified(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, "inspection returned no object")
}

// Verifies that container inspection without an ID cannot establish successful forced disconnection.
func TestForcedDisconnectRejectsMalformedContainerInspection(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.ErrorContains(t, disconnectErr, "inspection returned an empty ID")
}

// Verifies that a confirmed missing container is accepted as a successful forced-disconnect outcome.
func TestForcedDisconnectAcceptsGenuineMissingContainer(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"Container 'container' not found.\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[]`,
		"Container 'container' not found.\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.NoError(t, disconnectErr)
}

// Verifies that a confirmed missing network after a detach attempt is accepted as a successful forced-disconnect outcome.
func TestForcedDisconnectAcceptsGenuineMissingNetworkAfterDetach(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "disconnect", "network-name", "container"},
		"",
		"Network not found: 'network-name'\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container"},
		`[{"Id":"container-id","Name":"/container","NetworkSettings":{"Networks":{}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-name"},
		`[]`,
		"Network not found: 'network-name'\n",
		1,
	)

	disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
		Network:   "network-id",
		Container: "container",
		Force:     true,
	})

	require.NoError(t, disconnectErr)
}

// Verifies that network listing obtains labels through inspection so commas and equals signs in values are preserved.
func TestListNetworksUsesInspectionForAuthoritativeLabels(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "list", "--no-trunc", "--filter", "label=owner=dcp", "--format", "json"},
		`{"Driver":"bridge","ID":"network-id","IPv6":"false","Internal":"false","Labels":"owner=dcp,metadata={\"one\":1,\"two\":2}","Name":"network-name"}`+"\n",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "network-id"},
		`[{"Id":"network-id","Name":"network-name","Driver":"bridge","IPAM":{"Config":[]},"Labels":{"owner":"dcp","value":"one,two=three"},"Containers":{}}]`,
		"",
		0,
	)

	listed, listErr := orchestrator.ListNetworks(ctx, containers.ListNetworksOptions{
		Filters: containers.ListNetworksFilters{
			LabelFilters: []containers.LabelFilter{{Key: "owner", Value: "dcp"}},
		},
	})

	require.NoError(t, listErr)
	require.Len(t, listed, 1)
	require.Equal(t, "network-id", listed[0].ID)
	require.Equal(t, "one,two=three", listed[0].Labels["value"])
}

// Verifies that a network removed between list and label inspection is omitted without failing the list operation.
func TestListNetworksIgnoresRemovalDuringLabelInspection(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "list", "--no-trunc", "--format", "json"},
		"{\"ID\":\"first-id\",\"Name\":\"first\",\"Driver\":\"bridge\"}\n"+
			"{\"ID\":\"removed-id\",\"Name\":\"removed\",\"Driver\":\"bridge\"}\n",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "first-id", "removed-id"},
		`[{"Id":"first-id","Name":"first","Driver":"bridge","IPAM":{"Config":[]},"Labels":{"owner":"dcp"},"Containers":{}}]`,
		"Network not found: 'removed-id'\n",
		1,
	)

	listed, listErr := orchestrator.ListNetworks(ctx, containers.ListNetworksOptions{})

	require.NoError(t, listErr)
	require.Len(t, listed, 1)
	require.Equal(t, "first-id", listed[0].ID)
	require.Equal(t, "dcp", listed[0].Labels["owner"])
}

// Verifies that network removal retains successful requests while reporting missing networks and incomplete results.
func TestRemoveNetworksPreservesPartialResults(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "first"},
		`[{"Id":"first-id","Name":"first","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(t, executor, []string{"wslc", "network", "remove", "first"}, "first\n", "", 0)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "missing"},
		`[]`,
		"Network not found: 'missing'\n",
		1,
	)

	removed, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{"first", "missing"},
	})

	require.Equal(t, []string{"first"}, removed)
	require.True(t, errors.Is(removeErr, containers.ErrNotFound))
	require.True(t, errors.Is(removeErr, containers.ErrIncomplete))
}

// Verifies that bridge, host, and none are recognized as built-in WSLC networks while application networks are not.
func TestIsBuiltInNetwork(t *testing.T) {
	t.Parallel()

	orchestrator := &WslcCliOrchestrator{}
	require.True(t, orchestrator.IsBuiltInNetwork("bridge"))
	require.True(t, orchestrator.IsBuiltInNetwork("host"))
	require.True(t, orchestrator.IsBuiltInNetwork("none"))
	require.False(t, orchestrator.IsBuiltInNetwork("application"))
}
