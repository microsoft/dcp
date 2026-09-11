/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestInspectedContainerDeserialization(t *testing.T) {
	var b bytes.Buffer

	_, err := b.WriteString(inspectedConsulJune2024)
	require.NoError(t, err)

	inspectedContainers, err := asObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)

	ct := inspectedContainers[0]

	require.Equal(t, "cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1", ct.Id)
	require.Equal(t, "consul-d86ile8", ct.Name)
	require.Equal(t, "docker.io/hashicorp/consul:latest", ct.Image)
	expectedCreatedTime, err := time.Parse(time.RFC3339Nano, "2024-06-14T12:01:02.881754461-07:00")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, ct.CreatedAt)
	expectedStartedTime, err := time.Parse(time.RFC3339Nano, "2024-06-14T12:01:04.283842634-07:00")
	require.NoError(t, err)
	require.Equal(t, expectedStartedTime, ct.StartedAt)
	require.True(t, ct.FinishedAt.IsZero())
	require.Equal(t, containers.ContainerStatusRunning, ct.Status)
	require.EqualValues(t, 0, ct.ExitCode)

	require.Equal(t, containers.InspectedContainerPortMapping{
		// Only the ports that are mapped to the host are included
		"8500/tcp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "39133"}},
		"8600/udp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "39993"}},
	}, ct.Ports)

	require.Equal(t, map[string]string{
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container":       "podman",
		"BIN_NAME":        "consul",
		"PRODUCT_VERSION": "1.19.0",
		"PRODUCT_NAME":    "consul",
		"HOME":            "/root",
		"HOSTNAME":        "cf5947988412",
	}, ct.Env)

	require.Equal(t, []string{"docker-entrypoint.sh", "agent", "-dev", "-client", "0.0.0.0"}, ct.Args)

	require.Equal(t, []containers.InspectedContainerNetwork{
		{
			Id:         "podman",
			Name:       "podman",
			IPAddress:  "10.88.0.8",
			Gateway:    "10.88.0.1",
			MacAddress: "62:c3:4c:66:23:d3",
			Aliases:    []string{"cf5947988412"},
		},
	}, ct.Networks)
}

func TestUnmarshalInspectedNetworkReportsIPv6(t *testing.T) {
	t.Parallel()

	var network containers.InspectedNetwork
	unmarshalErr := unmarshalNetwork(&podmanInspectedNetwork{
		Id:         "network-id",
		Name:       "ipv6-network",
		Driver:     "bridge",
		EnableIPv6: true,
	}, &network)

	require.NoError(t, unmarshalErr)
	require.True(t, network.IPv6)
}

func TestApplyListContainersOptions(t *testing.T) {
	t.Parallel()

	args := applyListContainersOptions(
		[]string{"container", "ls", "--no-trunc"},
		containers.ListContainersOptions{
			All: true,
			Filters: containers.ListContainersFilters{
				LabelFilters:   []containers.LabelFilter{{Key: "owner", Value: "dcp"}},
				NetworkFilters: []string{"network-id"},
			},
		},
	)

	require.Equal(t, []string{
		"container", "ls", "--no-trunc", "--all",
		"--filter", "label=owner=dcp",
		"--filter", "network=network-id",
	}, args)
}

func TestUnmarshalListedContainerUsesFirstName(t *testing.T) {
	t.Parallel()

	var listed containers.ListedContainer
	unmarshalErr := unmarshalListedContainer(&podmanListedContainer{
		Id:    "container-id",
		Names: []string{"container-name", "alternate-name"},
	}, &listed)

	require.NoError(t, unmarshalErr)
	require.Equal(t, "container-id", listed.Id)
	require.Equal(t, "container-name", listed.Name)
}

func TestPodmanNetworkEventConversion(t *testing.T) {
	t.Parallel()

	orchestrator := &PodmanCliOrchestrator{
		networkIDs: make(map[string]networkIDCacheEntry),
	}
	orchestrator.rememberNetworkID("network-name", "network-id")

	var event podmanEventMessage
	unmarshalErr := json.Unmarshal([]byte(`{
		"ID": "container-id",
		"Name": "container-name",
		"Network": "network-name",
		"Status": "connect",
		"Type": "network",
		"Attributes": {"driver": "bridge"}
	}`), &event)
	require.NoError(t, unmarshalErr)

	converted, convertErr := orchestrator.toNetworkEventMessage(&event)
	require.NoError(t, convertErr)
	require.Equal(t, containers.EventSourceNetwork, converted.Source)
	require.Equal(t, containers.EventActionConnect, converted.Action)
	require.Equal(t, "network-id", converted.Actor.ID)
	require.Equal(t, "container-id", converted.Attributes["container"])
	require.Equal(t, "container-name", converted.Attributes["name"])
	require.Equal(t, "network-name", converted.Attributes["network"])
	require.Equal(t, "bridge", converted.Attributes["driver"])
}

func TestPodmanNetworkEventsArgsReplayFromWatchStart(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"events",
		"--since", "3.5s",
		"--filter", "type=network",
		"--format", "json",
	}, podmanNetworkEventsArgs(3500*time.Millisecond))
}

func TestPodmanNetworkRemoveEventIsNormalized(t *testing.T) {
	t.Parallel()

	orchestrator := &PodmanCliOrchestrator{
		networkIDs: make(map[string]networkIDCacheEntry),
	}
	event := podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "network-id",
		Name:    "network-name",
		Action:  containers.EventAction("remove"),
		Network: "network-name",
	}

	converted, convertErr := orchestrator.toNetworkEventMessage(&event)
	require.NoError(t, convertErr)
	require.Equal(t, containers.EventActionDestroy, converted.Action)
	require.Equal(t, "network-id", converted.Actor.ID)
}

func TestPodmanContainerRemoveEventIsNormalized(t *testing.T) {
	t.Parallel()

	event := podmanEventMessage{
		Source: containers.EventSourceContainer,
		ID:     "container-id",
		Name:   "container-name",
		Action: containers.EventActionRemove,
	}

	converted := event.ToEventMessage()
	require.Equal(t, containers.EventSourceContainer, converted.Source)
	require.Equal(t, containers.EventActionDestroy, converted.Action)
	require.Equal(t, "container-id", converted.Actor.ID)
	require.Equal(t, "container-name", converted.Attributes["name"])
}

func TestPodmanNetworkEventCacheHandlesNameReuse(t *testing.T) {
	t.Parallel()

	orchestrator := &PodmanCliOrchestrator{
		networkIDs: make(map[string]networkIDCacheEntry),
	}
	orchestrator.rememberNetworkID("network-name", "old-network-id")

	created, createErr := orchestrator.toNetworkEventMessage(&podmanEventMessage{
		Source: containers.EventSourceNetwork,
		ID:     "new-network-id",
		Name:   "network-name",
		Action: containers.EventActionCreate,
	})
	require.NoError(t, createErr)
	require.Equal(t, "new-network-id", created.Actor.ID)

	connected, connectErr := orchestrator.toNetworkEventMessage(&podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionConnect,
	})
	require.NoError(t, connectErr)
	require.Equal(t, "new-network-id", connected.Actor.ID)

	destroyed, destroyErr := orchestrator.toNetworkEventMessage(&podmanEventMessage{
		Source: containers.EventSourceNetwork,
		ID:     "new-network-id",
		Name:   "network-name",
		Action: containers.EventActionRemove,
	})
	require.NoError(t, destroyErr)
	require.Equal(t, containers.EventActionDestroy, destroyed.Action)
	require.Equal(t, "new-network-id", destroyed.Actor.ID)

	_, found := orchestrator.cachedNetworkID("network-name")
	require.False(t, found)
}

func TestCreateNetworkReturnsInspectedID(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "create", "network-name"},
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 0
		},
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "inspect", "--format", "json", "network-name"},
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte(`[{"name":"network-name","id":"network-id"}]`))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	networkID, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: "network-name",
	})

	require.NoError(t, createErr)
	require.Equal(t, "network-id", networkID)
	require.Len(t, executor.FindAll([]string{"podman", "network", "create", "network-name"}, "", nil), 1)
	require.Len(t, executor.FindAll([]string{"podman", "network", "inspect", "--format", "json", "network-name"}, "", nil), 1)
	cachedNetworkID, found := orchestrator.(*PodmanCliOrchestrator).cachedNetworkID("network-name")
	require.True(t, found)
	require.Equal(t, "network-id", cachedNetworkID)
}

func TestListNetworksRemembersIDs(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "ls", "--format", "json"},
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte(`[{"name":"network-name","id":"network-id"}]`))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor).(*PodmanCliOrchestrator)
	networks, listErr := orchestrator.ListNetworks(ctx, containers.ListNetworksOptions{})

	require.NoError(t, listErr)
	require.Len(t, networks, 1)
	cachedNetworkID, found := orchestrator.cachedNetworkID("network-name")
	require.True(t, found)
	require.Equal(t, "network-id", cachedNetworkID)
}

func TestBuildImageUsesIIDFile(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	expectedCommand := []string{
		"podman", "build",
		"--iidfile", "image.iid",
		"-t", "image:tag",
		"context",
	}
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: expectedCommand,
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		IidFile: "image.iid",
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context: "context",
			Tags:    []string{"image:tag"},
		},
	})

	require.NoError(t, buildErr)
	require.Len(t, executor.FindAll(expectedCommand, "", nil), 1)
}

func TestPullImageAllowsInsecureRegistry(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	expectedCommand := []string{
		"podman", "image", "pull", "--quiet", "--tls-verify=false", "localhost:5000/test/image:latest",
	}
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: expectedCommand,
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte("sha256:image-id\n"))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{
		Image:                 "localhost:5000/test/image:latest",
		AllowInsecureRegistry: true,
	})

	require.NoError(t, pullErr)
	require.Equal(t, "sha256:image-id", imageID)
	require.Len(t, executor.FindAll(expectedCommand, "", nil), 1)
}

func TestPullImageVerifiesRegistryTLSByDefault(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	expectedCommand := []string{
		"podman", "image", "pull", "--quiet", "example.test/test/image:latest",
	}
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: expectedCommand,
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte("sha256:image-id\n"))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{
		Image: "example.test/test/image:latest",
	})

	require.NoError(t, pullErr)
	require.Equal(t, "sha256:image-id", imageID)
	require.Len(t, executor.FindAll(expectedCommand, "", nil), 1)
}

func TestResolveNetworkEventMessageInspectsCacheMiss(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "inspect", "--format", "json", "network-name"},
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte(`[{"name":"network-name","id":"network-id"}]`))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor).(*PodmanCliOrchestrator)
	_, cacheMissErr, resolution := orchestrator.normalizeNetworkEventMessage(&podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionConnect,
	})
	require.ErrorIs(t, cacheMissErr, errNetworkIDNotCached)
	require.NotNil(t, resolution)
	message, resolveErr := orchestrator.resolveNetworkEventMessage(ctx, &podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionConnect,
	}, *resolution)

	require.NoError(t, resolveErr)
	require.Equal(t, "network-id", message.Actor.ID)
	require.Equal(t, "container-id", message.Attributes["container"])
	require.Len(t, executor.FindAll([]string{"podman", "network", "inspect", "--format", "json", "network-name"}, "", nil), 1)
}

func TestResolveNetworkEventMessageDoesNotOverwriteNewerCacheEntry(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	inspectEntered := make(chan struct{})
	releaseInspect := make(chan struct{})
	var releaseInspectOnce sync.Once
	releaseInspectFunc := func() {
		releaseInspectOnce.Do(func() {
			close(releaseInspect)
		})
	}
	t.Cleanup(releaseInspectFunc)
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "inspect", "--format", "json", "network-name"},
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			close(inspectEntered)
			<-releaseInspect
			_, writeErr := execution.Cmd.Stdout.Write([]byte(`[{"name":"network-name","id":"old-network-id"}]`))
			require.NoError(t, writeErr)
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor).(*PodmanCliOrchestrator)
	event := podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionConnect,
	}
	_, cacheMissErr, resolution := orchestrator.normalizeNetworkEventMessage(&event)
	require.ErrorIs(t, cacheMissErr, errNetworkIDNotCached)
	require.NotNil(t, resolution)

	type resolutionResult struct {
		message containers.EventMessage
		err     error
	}
	resolutionResults := make(chan resolutionResult, 1)
	go func() {
		message, resolveErr := orchestrator.resolveNetworkEventMessage(ctx, &event, *resolution)
		resolutionResults <- resolutionResult{message: message, err: resolveErr}
	}()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-inspectEntered:
	}

	created, createErr := orchestrator.toNetworkEventMessage(&podmanEventMessage{
		Source: containers.EventSourceNetwork,
		ID:     "new-network-id",
		Name:   "network-name",
		Action: containers.EventActionCreate,
	})
	require.NoError(t, createErr)
	require.Equal(t, "new-network-id", created.Actor.ID)
	releaseInspectFunc()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case result := <-resolutionResults:
		require.Error(t, result.err)
		require.Equal(t, "network-name", result.message.Actor.ID)
	}
	cachedNetworkID, found := orchestrator.cachedNetworkID("network-name")
	require.True(t, found)
	require.Equal(t, "new-network-id", cachedNetworkID)
}

func TestResolveAndNotifyNetworkEventDeliversUnresolvedEventOnFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "network", "inspect", "--format", "json", "network-name"},
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 1
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor).(*PodmanCliOrchestrator)
	subscriptions := pubsub.NewSubscriptionSet[containers.EventMessage](nil, t.Context())
	events := make(chan containers.EventMessage, 1)
	subscription := subscriptions.Subscribe(events)
	t.Cleanup(subscription.Cancel)
	orchestrator.networkEventResolutionSlots <- struct{}{}
	_, cacheMissErr, resolution := orchestrator.normalizeNetworkEventMessage(&podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionDisconnect,
	})
	require.ErrorIs(t, cacheMissErr, errNetworkIDNotCached)
	require.NotNil(t, resolution)

	orchestrator.resolveAndNotifyNetworkEvent(ctx, subscriptions, podmanEventMessage{
		Source:  containers.EventSourceNetwork,
		ID:      "container-id",
		Network: "network-name",
		Action:  containers.EventActionDisconnect,
	}, "event data", *resolution)

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case event := <-events:
		require.Equal(t, "network-name", event.Actor.ID)
		require.Equal(t, "container-id", event.Attributes["container"])
	}
	require.Empty(t, orchestrator.networkEventResolutionSlots)
}

func TestBackgroundStatusUpdatesRefresh(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "container", "ls", "--last", "1", "--quiet"},
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 0
		},
	})
	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	backgroundCtx, backgroundCancel := context.WithCancel(ctx)
	defer backgroundCancel()
	orchestrator.EnsureBackgroundStatusUpdates(backgroundCtx)

	statusCommand := []string{"podman", "container", "ls", "--last", "1", "--quiet"}
	_, refreshErr := internal_testutil.WaitForCommand(executor, ctx, statusCommand, "", nil)
	require.NoError(t, refreshErr)

	status := orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	require.True(t, status.IsHealthy(), "expected healthy cached status: %+v", status)
	require.Len(t, executor.FindAll(statusCommand, "", nil), 1)
}

func TestRemoveImagesReturnsRequestedIdentifiers(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"podman", "image", "rm", "--force"},
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 0
		},
	})

	orchestrator := NewPodmanCliOrchestrator(testr.New(t), executor)
	requested := []string{"example.test/first:latest", "sha256:0123456789"}
	removed, removeErr := orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
		Images: requested,
		Force:  true,
	})

	require.NoError(t, removeErr)
	require.Equal(t, requested, removed)
	require.Len(t, executor.FindAll([]string{"podman", "image", "rm", "--force"}, "", nil), len(requested))
}

func TestIsBuiltInNetwork(t *testing.T) {
	t.Parallel()

	orchestrator := &PodmanCliOrchestrator{}
	require.True(t, orchestrator.IsBuiltInNetwork("podman"))
	require.False(t, orchestrator.IsBuiltInNetwork("bridge"))
	require.False(t, orchestrator.IsBuiltInNetwork("host"))
	require.False(t, orchestrator.IsBuiltInNetwork("none"))
	require.False(t, orchestrator.IsBuiltInNetwork("application"))
}

func TestApplyCreateContainerOptionsVolumeMounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mount    containers.CreateContainerVolumeMount
		wantArgs []string
	}{
		{
			name: "named volume includes src",
			mount: containers.CreateContainerVolumeMount{
				Type:   containers.NamedVolumeMount,
				Source: "myvolume",
				Target: "/data",
			},
			wantArgs: []string{"--mount", "type=volume,src=myvolume,target=/data"},
		},
		{
			name: "anonymous volume omits src",
			mount: containers.CreateContainerVolumeMount{
				Type:   containers.NamedVolumeMount,
				Source: "",
				Target: "/data",
			},
			wantArgs: []string{"--mount", "type=volume,target=/data"},
		},
		{
			name: "named volume readonly",
			mount: containers.CreateContainerVolumeMount{
				Type:     containers.NamedVolumeMount,
				Source:   "myvolume",
				Target:   "/data",
				ReadOnly: true,
			},
			wantArgs: []string{"--mount", "type=volume,src=myvolume,target=/data,readonly"},
		},
		{
			name: "anonymous volume readonly",
			mount: containers.CreateContainerVolumeMount{
				Type:     containers.NamedVolumeMount,
				Source:   "",
				Target:   "/data",
				ReadOnly: true,
			},
			wantArgs: []string{"--mount", "type=volume,target=/data,readonly"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := containers.CreateContainerOptions{}
			options.VolumeMounts = []containers.CreateContainerVolumeMount{tc.mount}
			args := applyCreateContainerOptions([]string{}, options)
			require.Equal(t, tc.wantArgs, args)
		})
	}
}

func TestApplyCreateContainerOptionsNetworks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  containers.CreateContainerOptions
		wantArgs []string
	}{
		{
			name: "single network without aliases",
			options: containers.CreateContainerOptions{
				Networks: []containers.CreateContainerNetworkOptions{
					{Name: "network-a"},
				},
			},
			wantArgs: []string{"--network", "network-a"},
		},
		{
			name: "single network with aliases",
			options: containers.CreateContainerOptions{
				Networks: []containers.CreateContainerNetworkOptions{
					{
						Name:    "network-a",
						Aliases: []string{"alias-a", "alias-b"},
					},
				},
			},
			wantArgs: []string{"--network", "network-a:alias=alias-a,alias=alias-b"},
		},
		{
			name: "multiple networks with aliases",
			options: containers.CreateContainerOptions{
				Networks: []containers.CreateContainerNetworkOptions{
					{
						Name:    "network-a",
						Aliases: []string{"alias-a", "alias-b"},
					},
					{
						Name:    "network-b",
						Aliases: []string{"alias-c"},
					},
				},
			},
			wantArgs: []string{
				"--network", "network-a:alias=alias-a,alias=alias-b",
				"--network", "network-b:alias=alias-c",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := applyCreateContainerOptions([]string{}, tc.options)
			require.Equal(t, tc.wantArgs, args)
		})
	}
}

const inspectedConsulJune2024 = `
[
      {
          "Id": "cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1",
          "Created": "2024-06-14T12:01:02.881754461-07:00",
          "Path": "docker-entrypoint.sh",
          "Args": [
               "agent",
               "-dev",
               "-client",
               "0.0.0.0"
          ],
          "State": {
               "OciVersion": "1.2.0",
               "Status": "running",
               "Running": true,
               "Paused": false,
               "Restarting": false,
               "OOMKilled": false,
               "Dead": false,
               "Pid": 12153,
               "ConmonPid": 12146,
               "ExitCode": 0,
               "Error": "",
               "StartedAt": "2024-06-14T12:01:04.283842634-07:00",
               "FinishedAt": "0001-01-01T00:00:00Z",
               "CgroupPath": "/libpod_parent/libpod-cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1",
               "CheckpointedAt": "0001-01-01T00:00:00Z",
               "RestoredAt": "0001-01-01T00:00:00Z"
          },
          "Image": "bb7114bcaf5225329144303e67841fd4613bd9328d5f0d516db08799b23a7f2a",
          "ImageDigest": "sha256:05baa180b8a505d1bbe60725920544f8ad74bea1f82f45a2c7d89f79b02721ed",
          "ImageName": "docker.io/hashicorp/consul:latest",
          "Rootfs": "",
          "Pod": "",
          "ResolvConfPath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/resolv.conf",
          "HostnamePath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/hostname",
          "HostsPath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/hosts",
          "StaticDir": "/var/lib/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata",
          "OCIConfigPath": "/var/lib/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/config.json",
          "OCIRuntime": "crun",
          "ConmonPidFile": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/conmon.pid",
          "PidFile": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/pidfile",
          "Name": "consul-d86ile8",
          "RestartCount": 0,
          "Driver": "overlay",
          "MountLabel": "",
          "ProcessLabel": "",
          "AppArmorProfile": "",
          "EffectiveCaps": [
               "CAP_CHOWN",
               "CAP_DAC_OVERRIDE",
               "CAP_FOWNER",
               "CAP_FSETID",
               "CAP_KILL",
               "CAP_NET_BIND_SERVICE",
               "CAP_SETFCAP",
               "CAP_SETGID",
               "CAP_SETPCAP",
               "CAP_SETUID",
               "CAP_SYS_CHROOT"
          ],
          "BoundingCaps": [
               "CAP_CHOWN",
               "CAP_DAC_OVERRIDE",
               "CAP_FOWNER",
               "CAP_FSETID",
               "CAP_KILL",
               "CAP_NET_BIND_SERVICE",
               "CAP_SETFCAP",
               "CAP_SETGID",
               "CAP_SETPCAP",
               "CAP_SETUID",
               "CAP_SYS_CHROOT"
          ],
          "ExecIDs": [],
          "GraphDriver": {
               "Name": "overlay",
               "Data": {
                    "LowerDir": "/var/lib/containers/storage/overlay/0ebba15ea4e7eda05faa9f8e199f128f0218dc704b76189e79ab8e45be1684b4/diff:/var/lib/containers/storage/overlay/ad789f5ddef99a9c34b66a238fe4432f66961206e6a4689473554df9d3d441c6/diff:/var/lib/containers/storage/overlay/4eeb1a474633de44192a9cbdf44b43881064119529a569c550226ab2aabcae7c/diff:/var/lib/containers/storage/overlay/f0dd272e72bca22d3b58721b0451d43533527eb69c8022f69e878207650a4417/diff:/var/lib/containers/storage/overlay/08c74feafbfd0ed9b5fd08b120e888fa06c78f36432fa67af4eeb99259e2799c/diff:/var/lib/containers/storage/overlay/f15694bde2ada692fe0faa088896d6b06028d1fe80caa086350df9be15d9b37b/diff:/var/lib/containers/storage/overlay/bc29d22c7eb7c1d866c31f66defac8af9642964c77f3f1c1e8a2af33b55b24b5/diff:/var/lib/containers/storage/overlay/5fc209161df7fe8003405fb262d556af088c26785b99e102d9bcb653f0915f10/diff:/var/lib/containers/storage/overlay/d4fc045c9e3a848011de66f34b81f052d4f2c15a17bb196d637e526349601820/diff",
                    "MergedDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/merged",
                    "UpperDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/diff",
                    "WorkDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/work"
               }
          },
          "Mounts": [
               {
                    "Type": "volume",
                    "Name": "68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a",
                    "Source": "/var/lib/containers/storage/volumes/68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a/_data",
                    "Destination": "/consul/data",
                    "Driver": "local",
                    "Mode": "",
                    "Options": [
                         "nodev",
                         "exec",
                         "nosuid",
                         "rbind"
                    ],
                    "RW": true,
                    "Propagation": "rprivate"
               }
          ],
          "Dependencies": [],
          "NetworkSettings": {
               "EndpointID": "",
               "Gateway": "10.88.0.1",
               "IPAddress": "10.88.0.8",
               "IPPrefixLen": 16,
               "IPv6Gateway": "",
               "GlobalIPv6Address": "",
               "GlobalIPv6PrefixLen": 0,
               "MacAddress": "62:c3:4c:66:23:d3",
               "Bridge": "",
               "SandboxID": "",
               "HairpinMode": false,
               "LinkLocalIPv6Address": "",
               "LinkLocalIPv6PrefixLen": 0,
               "Ports": {
                    "8300/tcp": null,
                    "8301/tcp": null,
                    "8301/udp": null,
                    "8302/tcp": null,
                    "8302/udp": null,
                    "8500/tcp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39133"
                         }
                    ],
                    "8600/tcp": null,
                    "8600/udp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39993"
                         }
                    ]
               },
               "SandboxKey": "/run/netns/netns-802a14fb-7d36-bf94-60d9-ec07a6bf9560",
               "Networks": {
                    "podman": {
                         "EndpointID": "",
                         "Gateway": "10.88.0.1",
                         "IPAddress": "10.88.0.8",
                         "IPPrefixLen": 16,
                         "IPv6Gateway": "",
                         "GlobalIPv6Address": "",
                         "GlobalIPv6PrefixLen": 0,
                         "MacAddress": "62:c3:4c:66:23:d3",
                         "NetworkID": "podman",
                         "DriverOpts": null,
                         "IPAMConfig": null,
                         "Links": null,
                         "Aliases": [
                              "cf5947988412"
                         ]
                    }
               }
          },
          "Namespace": "",
          "IsInfra": false,
          "IsService": false,
          "KubeExitCodePropagation": "invalid",
          "lockNumber": 9,
          "Config": {
               "Hostname": "cf5947988412",
               "Domainname": "",
               "User": "",
               "AttachStdin": false,
               "AttachStdout": false,
               "AttachStderr": false,
               "Tty": false,
               "OpenStdin": false,
               "StdinOnce": false,
               "Env": [
                    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                    "container=podman",
                    "BIN_NAME=consul",
                    "PRODUCT_VERSION=1.19.0",
                    "PRODUCT_NAME=consul",
                    "HOME=/root",
                    "HOSTNAME=cf5947988412"
               ],
               "Cmd": [
                    "agent",
                    "-dev",
                    "-client",
                    "0.0.0.0"
               ],
               "Image": "docker.io/hashicorp/consul:latest",
               "Volumes": null,
               "WorkingDir": "/",
               "Entrypoint": [
                    "docker-entrypoint.sh"
               ],
               "OnBuild": null,
               "Labels": {
                    "org.opencontainers.image.authors": "Consul Team \u003cconsul@hashicorp.com\u003e",
                    "org.opencontainers.image.description": "Consul is a datacenter runtime that provides service discovery, configuration, and orchestration.",
                    "org.opencontainers.image.documentation": "https://www.consul.io/docs",
                    "org.opencontainers.image.licenses": "BSL-1.1",
                    "org.opencontainers.image.source": "https://github.com/hashicorp/consul",
                    "org.opencontainers.image.title": "consul",
                    "org.opencontainers.image.url": "https://www.consul.io/",
                    "org.opencontainers.image.vendor": "HashiCorp",
                    "org.opencontainers.image.version": "1.19.0",
                    "version": "1.19.0"
               },
               "Annotations": {
                    "io.container.manager": "libpod",
                    "org.opencontainers.image.stopSignal": "15"
               },
               "StopSignal": "SIGTERM",
               "HealthcheckOnFailureAction": "none",
               "CreateCommand": [
                    "C:\\Users\\karolz\\scoop\\apps\\podman\\current\\podman.exe",
                    "create",
                    "--name",
                    "consul-d86ile8",
                    "-p",
                    "127.0.0.1::8500/TCP",
                    "-p",
                    "127.0.0.1::8600/UDP",
                    "docker.io/hashicorp/consul:latest"
               ],
               "Umask": "0022",
               "Timeout": 0,
               "StopTimeout": 10,
               "Passwd": true,
               "sdNotifyMode": "container"
          },
          "HostConfig": {
               "Binds": [
                    "68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a:/consul/data:rprivate,rw,nodev,exec,nosuid,rbind"
               ],
               "CgroupManager": "cgroupfs",
               "CgroupMode": "host",
               "ContainerIDFile": "",
               "LogConfig": {
                    "Type": "journald",
                    "Config": null,
                    "Path": "",
                    "Tag": "",
                    "Size": "0B"
               },
               "NetworkMode": "bridge",
               "PortBindings": {
                    "8500/tcp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39133"
                         }
                    ],
                    "8600/udp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39993"
                         }
                    ]
               },
               "RestartPolicy": {
                    "Name": "no",
                    "MaximumRetryCount": 0
               },
               "AutoRemove": false,
               "Annotations": {
                    "io.container.manager": "libpod",
                    "org.opencontainers.image.stopSignal": "15"
               },
               "VolumeDriver": "",
               "VolumesFrom": null,
               "CapAdd": [],
               "CapDrop": [],
               "Dns": [],
               "DnsOptions": [],
               "DnsSearch": [],
               "ExtraHosts": [],
               "GroupAdd": [],
               "IpcMode": "shareable",
               "Cgroup": "",
               "Cgroups": "default",
               "Links": null,
               "OomScoreAdj": 0,
               "PidMode": "private",
               "Privileged": false,
               "PublishAllPorts": false,
               "ReadonlyRootfs": false,
               "SecurityOpt": [],
               "Tmpfs": {},
               "UTSMode": "private",
               "UsernsMode": "",
               "ShmSize": 65536000,
               "Runtime": "oci",
               "ConsoleSize": [
                    0,
                    0
               ],
               "Isolation": "",
               "CpuShares": 0,
               "Memory": 0,
               "NanoCpus": 0,
               "CgroupParent": "",
               "BlkioWeight": 0,
               "BlkioWeightDevice": null,
               "BlkioDeviceReadBps": null,
               "BlkioDeviceWriteBps": null,
               "BlkioDeviceReadIOps": null,
               "BlkioDeviceWriteIOps": null,
               "CpuPeriod": 0,
               "CpuQuota": 0,
               "CpuRealtimePeriod": 0,
               "CpuRealtimeRuntime": 0,
               "CpusetCpus": "",
               "CpusetMems": "",
               "Devices": [],
               "DiskQuota": 0,
               "KernelMemory": 0,
               "MemoryReservation": 0,
               "MemorySwap": 0,
               "MemorySwappiness": 0,
               "OomKillDisable": false,
               "PidsLimit": 2048,
               "Ulimits": [
                    {
                         "Name": "RLIMIT_NPROC",
                         "Soft": 4194304,
                         "Hard": 4194304
                    }
               ],
               "CpuCount": 0,
               "CpuPercent": 0,
               "IOMaximumIOps": 0,
               "IOMaximumBandwidth": 0,
               "CgroupConf": null
          }
     }
]`
