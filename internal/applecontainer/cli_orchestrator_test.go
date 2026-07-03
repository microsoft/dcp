/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
)

// Captured from `container inspect` (Apple container CLI 1.0.0) for a running container
// with a published port, a named volume mount, and a read-only bind mount.
const inspectedRunningContainer = `[{"configuration":{"capAdd":[],"capDrop":[],"creationDate":"2026-06-12T15:56:39Z","dns":{"nameservers":[],"options":[],"searchDomains":[]},"id":"dcp-probe-1","image":{"descriptor":{"digest":"sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62","mediaType":"application/vnd.oci.image.index.v1+json","size":9218},"reference":"docker.io/library/alpine:latest"},"initProcess":{"arguments":["600"],"environment":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"executable":"sleep","rlimits":[],"supplementalGroups":[],"terminal":false,"user":{"id":{"gid":0,"uid":0}},"workingDirectory":"/"},"labels":{"usvc-dev.dcp/probe":"1"},"mounts":[{"destination":"/vol","options":[],"source":"/Users/tester/Library/Application Support/com.apple.container/volumes/dcp-probe-vol/volume.img","type":{"volume":{"cache":{"on":{}},"format":"ext4","name":"dcp-probe-vol","sync":{"fsync":{}}}}},{"destination":"/bindro","options":["ro"],"source":"/tmp/dcp-cptree","type":{"virtiofs":{}}}],"networks":[{"network":"default","options":{"hostname":"dcp-probe-1","mtu":1280}}],"platform":{"architecture":"arm64","os":"linux"},"publishedPorts":[{"containerPort":80,"count":1,"hostAddress":"127.0.0.1","hostPort":18099,"proto":"tcp"}],"publishedSockets":[],"readOnly":false,"resources":{"cpuOverhead":1,"cpus":4,"memoryInBytes":1073741824},"rosetta":false,"runtimeHandler":"container-runtime-linux","ssh":false,"sysctls":{},"useInit":false,"virtualization":false},"id":"dcp-probe-1","status":{"networks":[{"hostname":"dcp-probe-1","ipv4Address":"192.168.65.11/24","ipv4Gateway":"192.168.65.1","ipv6Address":"fd88:ed83:617:6ebb:f4d1:1bff:fe87:2438/64","macAddress":"f6:d1:1b:87:24:38","mtu":1280,"network":"default"}],"startedDate":"2026-06-12T15:56:40Z","state":"running"}}]`

// A container that was created but never started: state is "stopped" and there is no startedDate.
const inspectedCreatedContainer = `[{"configuration":{"creationDate":"2026-06-12T15:30:00Z","id":"created-ctr","image":{"descriptor":{"digest":"sha256:865b"},"reference":"docker.io/library/alpine:latest"},"initProcess":{"arguments":["5"],"environment":["PATH=/bin"],"executable":"sleep","terminal":false,"workingDirectory":"/"},"labels":{},"mounts":[],"networks":[{"network":"default","options":{"hostname":"created-ctr","mtu":1280}}],"publishedPorts":[]},"id":"created-ctr","status":{"networks":[],"state":"stopped"}}]`

// A container that ran and exited: state is "stopped" and startedDate is set.
const inspectedExitedContainer = `[{"configuration":{"creationDate":"2026-06-12T15:29:00Z","id":"exited-ctr","image":{"reference":"docker.io/library/alpine:latest"},"initProcess":{"arguments":["-c","exit 7"],"environment":[],"executable":"sh"},"labels":{},"mounts":[],"networks":[],"publishedPorts":[]},"id":"exited-ctr","status":{"networks":[],"startedDate":"2026-06-12T15:29:03Z","state":"stopped"}}]`

// Captured from `container network inspect default`.
const inspectedDefaultNetwork = `[{"configuration":{"creationDate":"2026-06-12T12:18:16Z","labels":{"com.apple.container.resource.role":"builtin"},"mode":"nat","name":"default","options":{},"plugin":"container-network-vmnet"},"id":"default","status":{"ipv4Gateway":"192.168.65.1","ipv4Subnet":"192.168.65.0/24","ipv6Subnet":"fd88:ed83:617:6ebb::/64"}}]`

// Captured from `container volume inspect`.
const inspectedVolume = `[{"configuration":{"creationDate":"2026-06-12T12:28:56Z","driver":"local","format":"ext4","labels":{"some-label":"some-value"},"name":"my-volume","options":{},"sizeInBytes":549755813888,"source":"/Users/tester/Library/Application Support/com.apple.container/volumes/my-volume/volume.img"},"id":"my-volume"}]`

// Captured (abbreviated) from `container image inspect`.
const inspectedImage = `[{"configuration":{"creationDate":"2025-12-18T00:11:34Z","descriptor":{"digest":"sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62","mediaType":"application/vnd.oci.image.index.v1+json","size":9218},"name":"docker.io/library/alpine:latest"},"id":"865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62","variants":[{"config":{"architecture":"arm64","config":{"Cmd":["/bin/sh"],"Labels":{"image-label":"image-value"}},"os":"linux"}}]}]`

func TestInspectedContainerDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedRunningContainer)
	require.NoError(t, err)

	inspectedContainers, err := asArrayOfObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)

	ct := inspectedContainers[0]

	require.Equal(t, "dcp-probe-1", ct.Id)
	require.Equal(t, "dcp-probe-1", ct.Name)
	require.Equal(t, "docker.io/library/alpine:latest", ct.Image)

	expectedCreatedTime, err := time.Parse(time.RFC3339, "2026-06-12T15:56:39Z")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, ct.CreatedAt)
	expectedStartedTime, err := time.Parse(time.RFC3339, "2026-06-12T15:56:40Z")
	require.NoError(t, err)
	require.Equal(t, expectedStartedTime, ct.StartedAt)
	require.Equal(t, containers.ContainerStatusRunning, ct.Status)
	require.EqualValues(t, 0, ct.ExitCode)

	require.Equal(t, containers.InspectedContainerPortMapping{
		"80/tcp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "18099"}},
	}, ct.Ports)

	require.Equal(t, map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}, ct.Env)

	require.Equal(t, []string{"sleep", "600"}, ct.Args)

	require.Equal(t, []apiv1.VolumeMount{
		{
			Type:   apiv1.NamedVolumeMount,
			Source: "dcp-probe-vol",
			Target: "/vol",
		},
		{
			Type:     apiv1.BindMount,
			Source:   "/tmp/dcp-cptree",
			Target:   "/bindro",
			ReadOnly: true,
		},
	}, ct.Mounts)

	require.Equal(t, []containers.InspectedContainerNetwork{
		{
			Id:         "default",
			Name:       "default",
			IPAddress:  "192.168.65.11",
			Gateway:    "192.168.65.1",
			MacAddress: "f6:d1:1b:87:24:38",
			Aliases:    []string{"dcp-probe-1"},
		},
	}, ct.Networks)

	require.Equal(t, map[string]string{"usvc-dev.dcp/probe": "1"}, ct.Labels)
}

func TestInspectedContainerStateMapping(t *testing.T) {
	t.Parallel()

	// A created (never started) container reports as "created"
	var b bytes.Buffer
	_, err := b.WriteString(inspectedCreatedContainer)
	require.NoError(t, err)

	inspectedContainers, err := asArrayOfObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)
	require.Equal(t, containers.ContainerStatusCreated, inspectedContainers[0].Status)

	// The created container is not running, so network information comes from the configuration
	require.Equal(t, []containers.InspectedContainerNetwork{
		{
			Id:      "default",
			Name:    "default",
			Aliases: []string{"created-ctr"},
		},
	}, inspectedContainers[0].Networks)

	// A container that ran and stopped reports as "exited"
	b.Reset()
	_, err = b.WriteString(inspectedExitedContainer)
	require.NoError(t, err)

	inspectedContainers, err = asArrayOfObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)
	require.Equal(t, containers.ContainerStatusExited, inspectedContainers[0].Status)
}

func TestListedContainerDeserializationAndFiltering(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedRunningContainer)
	require.NoError(t, err)

	listed, err := asArrayOfObjects(&b, unmarshalListedContainer)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	lc := listed[0]
	require.Equal(t, "dcp-probe-1", lc.Id)
	require.Equal(t, "dcp-probe-1", lc.Name)
	require.Equal(t, "docker.io/library/alpine:latest", lc.Image)
	require.Equal(t, containers.ContainerStatusRunning, lc.Status)
	require.Equal(t, map[string]string{"usvc-dev.dcp/probe": "1"}, lc.Labels)
	require.Equal(t, []string{"default"}, lc.Networks)

	// Label filtering happens client-side
	getLabels := func(lc containers.ListedContainer) map[string]string { return lc.Labels }

	filtered := filterByLabels(listed, []containers.LabelFilter{{Key: "usvc-dev.dcp/probe", Value: "1"}}, getLabels)
	require.Len(t, filtered, 1)

	filtered = filterByLabels(listed, []containers.LabelFilter{{Key: "usvc-dev.dcp/probe"}}, getLabels)
	require.Len(t, filtered, 1)

	filtered = filterByLabels(listed, []containers.LabelFilter{{Key: "usvc-dev.dcp/probe", Value: "2"}}, getLabels)
	require.Empty(t, filtered)

	filtered = filterByLabels(listed, []containers.LabelFilter{{Key: "no-such-label"}}, getLabels)
	require.Empty(t, filtered)
}

func TestInspectedNetworkDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedDefaultNetwork)
	require.NoError(t, err)

	networks, err := asArrayOfObjects(&b, unmarshalNetwork)
	require.NoError(t, err)
	require.Len(t, networks, 1)

	net := networks[0]
	require.Equal(t, "default", net.Id)
	require.Equal(t, "default", net.Name)
	require.Equal(t, "container-network-vmnet", net.Driver)
	require.Equal(t, map[string]string{"com.apple.container.resource.role": "builtin"}, net.Labels)
	require.True(t, net.IPv6)
	require.Equal(t, []string{"192.168.65.0/24", "fd88:ed83:617:6ebb::/64"}, net.Subnets)
	require.Equal(t, []string{"192.168.65.1"}, net.Gateways)
}

func TestListedNetworkDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedDefaultNetwork)
	require.NoError(t, err)

	networks, err := asArrayOfObjects(&b, unmarshalListedNetwork)
	require.NoError(t, err)
	require.Len(t, networks, 1)

	net := networks[0]
	require.Equal(t, "default", net.ID)
	require.Equal(t, "default", net.Name)
	require.Equal(t, "container-network-vmnet", net.Driver)
	require.True(t, net.IPv6)
	require.Equal(t, map[string]string{"com.apple.container.resource.role": "builtin"}, net.Labels)
}

func TestInspectedVolumeDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedVolume)
	require.NoError(t, err)

	volumes, err := asArrayOfObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, volumes, 1)

	vol := volumes[0]
	require.Equal(t, "my-volume", vol.Name)
	require.Equal(t, "local", vol.Driver)
	require.Equal(t, "/Users/tester/Library/Application Support/com.apple.container/volumes/my-volume/volume.img", vol.MountPoint)
	require.Equal(t, map[string]string{"some-label": "some-value"}, vol.Labels)

	expectedCreatedTime, err := time.Parse(time.RFC3339, "2026-06-12T12:28:56Z")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, vol.CreatedAt)
}

func TestInspectedImageDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedImage)
	require.NoError(t, err)

	images, err := asArrayOfObjects(&b, unmarshalImage)
	require.NoError(t, err)
	require.Len(t, images, 1)

	img := images[0]
	require.Equal(t, "865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62", img.Id)
	require.Equal(t, []string{"docker.io/library/alpine:latest"}, img.Tags)
	require.Equal(t, "sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62", img.Digest)
	require.Equal(t, map[string]string{"image-label": "image-value"}, img.Labels)
}

func TestAsArrayOfObjectsRejectsNonArrayOutput(t *testing.T) {
	t.Parallel()

	// Docker/Podman produce JSON-lines output, which is not what the Apple container CLI produces.
	// Such output must be rejected so that runtime detection can recognize a mismatched CLI.
	var b bytes.Buffer
	_, err := b.WriteString("{\"Id\": \"abc\"}\n{\"Id\": \"def\"}\n")
	require.NoError(t, err)

	_, err = asArrayOfObjects(&b, unmarshalListedContainer)
	require.Error(t, err)
	require.ErrorIs(t, err, containers.ErrUnmarshalling)
}

func TestApplyCreateContainerOptions(t *testing.T) {
	t.Parallel()

	aco := &AppleContainerCliOrchestrator{log: logr.Discard()}

	options := containers.CreateContainerOptions{
		Name: "my-container",
		// Multiple requested networks resolve to the same runtime network on this runtime
		// (see UsesSingleNetwork) and must be deduplicated; aliases are not supported.
		Networks: []containers.CreateContainerNetworkOptions{
			{Name: "default"},
			{Name: "default", Aliases: []string{"alias1"}},
		},
		ContainerSpec: apiv1.ContainerSpec{
			VolumeMounts: []apiv1.VolumeMount{
				{Type: apiv1.NamedVolumeMount, Source: "myvolume", Target: "/data"},
				{Type: apiv1.BindMount, Source: "/host/path", Target: "/container/path", ReadOnly: true},
			},
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 8080, HostPort: 18080},
				{ContainerPort: 9090, HostPort: 19090, HostIP: "0.0.0.0", Protocol: apiv1.UDP},
			},
			Env: []apiv1.EnvVar{
				{Name: "FOO", Value: "bar"},
			},
			EnvFiles: []string{"/some/env/file.env"},
			Labels: []apiv1.ContainerLabel{
				{Key: "label1", Value: "value1"},
			},
			Command: "/custom/entrypoint",
			RunArgs: []string{"--rosetta"},
		},
	}

	args := aco.applyCreateContainerOptions([]string{"create"}, options)

	require.Equal(t, []string{
		"create",
		"--name", "my-container",
		"--network", "default",
		"--mount", "type=volume,source=myvolume,target=/data",
		"--mount", "type=bind,source=/host/path,target=/container/path,readonly",
		"-p", "127.0.0.1:18080:8080",
		"-p", "0.0.0.0:19090:9090/udp",
		"-e", "FOO=bar",
		"--env-file", "/some/env/file.env",
		"--label", "label1=value1",
		"--entrypoint", "/custom/entrypoint",
		"--rosetta",
	}, args)
}

func TestApplyCreateContainerOptionsOmitsUnsupportedFlags(t *testing.T) {
	t.Parallel()

	aco := &AppleContainerCliOrchestrator{log: logr.Discard()}

	terminal := apiv1.TerminalSpec{}
	options := containers.CreateContainerOptions{
		Name:     "my-container",
		Networks: []containers.CreateContainerNetworkOptions{{Name: "default", Aliases: []string{"alias1"}}},
		Healthcheck: containers.ContainerHealthcheck{
			Command: []string{"CMD", "echo", "ok"},
		},
		ContainerSpec: apiv1.ContainerSpec{
			RestartPolicy: apiv1.RestartPolicyAlways,
			PullPolicy:    apiv1.PullPolicyAlways,
			Terminal:      &terminal,
		},
	}

	args := aco.applyCreateContainerOptions([]string{"create"}, options)

	// Restart policies, pull policies, health checks, and network aliases are not supported by
	// the Apple container runtime and must not be emitted. A requested terminal maps to -it.
	require.Equal(t, []string{
		"create",
		"--name", "my-container",
		"--network", "default",
		"-it",
	}, args)
}

func TestMapContainerState(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)

	require.Equal(t, containers.ContainerStatusRunning, mapContainerState("running", started))
	require.Equal(t, containers.ContainerStatusExited, mapContainerState("stopped", started))
	require.Equal(t, containers.ContainerStatusCreated, mapContainerState("stopped", time.Time{}))
	require.Equal(t, containers.ContainerStatusRemoving, mapContainerState("stopping", started))
	require.Equal(t, containers.ContainerStatus("hibernating"), mapContainerState("hibernating", started))
}

func TestUnmarshalDiagnostics(t *testing.T) {
	t.Parallel()

	statusJson := `{"apiServerAppName":"container-apiserver","apiServerBuild":"release","apiServerCommit":"unspecified","apiServerVersion":"container-apiserver version 1.0.0 (build: release, commit: unspeci)","appRoot":"/Users/tester/Library/Application Support/com.apple.container/","installRoot":"/opt/homebrew/Cellar/container/1.0.0_1/","status":"running"}`

	var diagnostics containers.ContainerDiagnostics
	err := unmarshalDiagnostics([]byte(statusJson), &diagnostics)
	require.NoError(t, err)
	require.Equal(t, "1.0.0", diagnostics.ServerVersion)
}

func TestExtractVersion(t *testing.T) {
	t.Parallel()

	require.Equal(t, "1.0.0", extractVersion("container CLI version 1.0.0 (build: release, commit: unspeci)"))
	require.Equal(t, "2.1.3-beta", extractVersion("container-apiserver version 2.1.3-beta"))
	require.Equal(t, "unparseable", extractVersion(" unparseable "))
}

type recordingWriter struct {
	bytes.Buffer
}

func (w *recordingWriter) Sync() error  { return nil }
func (w *recordingWriter) Close() error { return nil }

func TestTimestampingWriter(t *testing.T) {
	t.Parallel()

	var inner recordingWriter
	w := newTimestampingWriter(&inner)

	_, err := w.Write([]byte("first line\nsecond "))
	require.NoError(t, err)
	_, err = w.Write([]byte("line\n"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(inner.String(), "\n"), "\n")
	require.Len(t, lines, 2)

	for i, expectedSuffix := range []string{"first line", "second line"} {
		timestamp, content, found := strings.Cut(lines[i], " ")
		require.True(t, found)
		_, parseErr := time.Parse(time.RFC3339Nano, timestamp)
		require.NoError(t, parseErr, "line %d should start with a RFC3339Nano timestamp, got: %s", i, lines[i])
		require.Equal(t, expectedSuffix, content)
	}
}

func TestStripCidrSuffix(t *testing.T) {
	t.Parallel()

	require.Equal(t, "192.168.65.11", stripCidrSuffix("192.168.65.11/24"))
	require.Equal(t, "192.168.65.11", stripCidrSuffix("192.168.65.11"))
	require.Equal(t, "", stripCidrSuffix(""))
}
