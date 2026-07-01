/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
)

func TestInspectedRunningContainerDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedRunningContainer)
	require.NoError(t, err)

	inspectedContainers, err := asObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)

	ct := inspectedContainers[0]

	require.Equal(t, "2187976cbf06c74c0582a4bd83bcfe50e6d7c268b753dc81130062868bb39320", ct.Id)
	require.Equal(t, "gleaming_scandinavian", ct.Name)
	require.Equal(t, "mcr.microsoft.com/dotnet/aspire-dashboard:latest", ct.Image)

	expectedCreatedTime, err := time.Parse(time.RFC3339Nano, "2026-06-30T10:00:53.872669951Z")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, ct.CreatedAt)

	expectedStartedTime, err := time.Parse(time.RFC3339Nano, "2026-06-30T10:00:54.32029315Z")
	require.NoError(t, err)
	require.Equal(t, expectedStartedTime, ct.StartedAt)

	require.Equal(t, containers.ContainerStatusRunning, ct.Status)
	require.EqualValues(t, 0, ct.ExitCode)

	// wslc reports port bindings at the top level of the inspect output.
	require.Equal(t, containers.InspectedContainerPortMapping{
		"18888/tcp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "18888"}},
		"18889/tcp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "4317"}},
	}, ct.Ports)

	require.Equal(t, "1654", ct.Env["APP_UID"])
	require.Equal(t, []string{"dotnet", "/app/Aspire.Dashboard.dll"}, ct.Args)

	// wslc does not report a network ID, so it is left empty.
	require.Equal(t, []containers.InspectedContainerNetwork{
		{
			Name:       "bridge",
			IPAddress:  "172.17.0.2",
			Gateway:    "172.17.0.1",
			MacAddress: "02:42:ac:11:00:02",
			Aliases:    []string{},
		},
	}, ct.Networks)
}

func TestInspectedExitedContainerDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedExitedContainer)
	require.NoError(t, err)

	inspectedContainers, err := asObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)

	ct := inspectedContainers[0]
	require.Equal(t, containers.ContainerStatusExited, ct.Status)
	require.EqualValues(t, 143, ct.ExitCode)
	require.False(t, ct.FinishedAt.IsZero())
}

func TestListedContainerDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(listedContainers)
	require.NoError(t, err)

	listed, err := asObjects(&b, unmarshalListedContainer)
	require.NoError(t, err)
	require.Len(t, listed, 3)

	require.Equal(t, containers.ContainerStatusCreated, listed[0].Status)
	require.Equal(t, containers.ContainerStatusRunning, listed[1].Status)
	require.Equal(t, containers.ContainerStatusExited, listed[2].Status)

	require.Equal(t, "gleaming_scandinavian", listed[1].Name)
	require.Equal(t, "mcr.microsoft.com/dotnet/aspire-dashboard:latest", listed[1].Image)
	require.Equal(t, "2187976cbf06c74c0582a4bd83bcfe50e6d7c268b753dc81130062868bb39320", listed[1].Id)
}

func TestWslcStateToStatus(t *testing.T) {
	t.Parallel()

	require.Equal(t, containers.ContainerStatusCreated, wslcStateToStatus(1))
	require.Equal(t, containers.ContainerStatusRunning, wslcStateToStatus(2))
	require.Equal(t, containers.ContainerStatusExited, wslcStateToStatus(3))
	require.Equal(t, containers.ContainerStatus(""), wslcStateToStatus(99))
}

func TestInspectedVolumeDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedVolume)
	require.NoError(t, err)

	volumes, err := asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, volumes, 1)

	vol := volumes[0]
	require.Equal(t, "dcp-testvol", vol.Name)
	require.Equal(t, "guest", vol.Driver)
	expectedCreatedTime, err := time.Parse(time.RFC3339, "2026-06-30T11:31:20Z")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, vol.CreatedAt)
}

func TestInspectedImageDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedImage)
	require.NoError(t, err)

	images, err := asObjects(&b, unmarshalImage)
	require.NoError(t, err)
	require.Len(t, images, 1)

	img := images[0]
	require.Equal(t, "sha256:a33c2e3a3c1f0b9e0e8c1f6f8b7a6d5c4b3a2918273645aef0123456789abcde", img.Id)
	require.Equal(t, []string{"mcr.microsoft.com/dotnet/aspire-dashboard:latest"}, img.Tags)
	require.Equal(t, "mcr.microsoft.com/dotnet/aspire-dashboard@sha256:1d584e3322bc9876543210fedcba0123456789abcdef0123456789abcdef0123", img.Digest)
}

func TestInspectedNetworkDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(inspectedNetwork)
	require.NoError(t, err)

	networks, err := asObjects(&b, unmarshalNetwork)
	require.NoError(t, err)
	require.Len(t, networks, 1)

	net := networks[0]
	require.Equal(t, "dcp-testnet", net.Id)
	require.Equal(t, "dcp-testnet", net.Name)
	require.Equal(t, "bridge", net.Driver)
	require.Equal(t, "local", net.Scope)
	require.False(t, net.Internal)
	require.True(t, net.Attachable)
	require.Equal(t, []string{"172.18.0.0/16"}, net.Subnets)
	require.Equal(t, []string{"172.18.0.1"}, net.Gateways)
	require.Equal(t, map[string]string{"com.microsoft.wsl.network.managed": "true"}, net.Labels)
}

func TestListedNetworkDeserialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := b.WriteString(listedNetworks)
	require.NoError(t, err)

	networks, err := asObjects(&b, unmarshalListedNetwork)
	require.NoError(t, err)
	require.Len(t, networks, 1)

	require.Equal(t, "dcp-testnet", networks[0].Name)
	require.Equal(t, "dcp-testnet", networks[0].ID)
	require.Equal(t, "bridge", networks[0].Driver)
}

func TestParseDiagnostics(t *testing.T) {
	t.Parallel()

	diag, err := parseDiagnostics([]byte("wslc 2.9.3.0\n"))
	require.NoError(t, err)
	require.Equal(t, "2.9.3.0", diag.ClientVersion)
	require.Equal(t, "2.9.3.0", diag.ServerVersion)

	_, err = parseDiagnostics([]byte("unexpected output"))
	require.Error(t, err)
}

func TestApplyCreateContainerOptionsVolumeMounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mount    apiv1.VolumeMount
		wantArgs []string
	}{
		{
			name: "named volume uses source:target",
			mount: apiv1.VolumeMount{
				Type:   apiv1.NamedVolumeMount,
				Source: "myvolume",
				Target: "/data",
			},
			wantArgs: []string{"-v", "myvolume:/data"},
		},
		{
			name: "anonymous volume uses target only",
			mount: apiv1.VolumeMount{
				Type:   apiv1.NamedVolumeMount,
				Source: "",
				Target: "/data",
			},
			wantArgs: []string{"-v", "/data"},
		},
		{
			name: "named volume readonly appends ro",
			mount: apiv1.VolumeMount{
				Type:     apiv1.NamedVolumeMount,
				Source:   "myvolume",
				Target:   "/data",
				ReadOnly: true,
			},
			wantArgs: []string{"-v", "myvolume:/data:ro"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := containers.CreateContainerOptions{}
			options.VolumeMounts = []apiv1.VolumeMount{tc.mount}
			args := applyCreateContainerOptions([]string{}, options)
			require.Equal(t, tc.wantArgs, args)
		})
	}
}

func TestApplyCreateContainerOptionsPorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		port     apiv1.ContainerPort
		wantArgs []string
	}{
		{
			name: "uppercase protocol is lowercased for wslc",
			port: apiv1.ContainerPort{
				HostPort:      5000,
				ContainerPort: 5000,
				Protocol:      apiv1.TCP,
			},
			wantArgs: []string{"-p", "127.0.0.1:5000:5000/tcp"},
		},
		{
			name: "udp protocol is lowercased",
			port: apiv1.ContainerPort{
				HostPort:      5300,
				ContainerPort: 53,
				Protocol:      apiv1.UDP,
			},
			wantArgs: []string{"-p", "127.0.0.1:5300:53/udp"},
		},
		{
			name: "no protocol omits the suffix",
			port: apiv1.ContainerPort{
				HostPort:      8080,
				ContainerPort: 80,
			},
			wantArgs: []string{"-p", "127.0.0.1:8080:80"},
		},
		{
			name: "host ip is honored",
			port: apiv1.ContainerPort{
				HostIP:        "0.0.0.0",
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      apiv1.TCP,
			},
			wantArgs: []string{"-p", "0.0.0.0:8080:80/tcp"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := containers.CreateContainerOptions{}
			options.Ports = []apiv1.ContainerPort{tc.port}
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
			name:     "single network with aliases",
			options:  containers.CreateContainerOptions{Network: "bridge", NetworkAliases: []string{"alias1", "alias2"}},
			wantArgs: []string{"--network", "bridge", "--network-alias", "alias1", "--network-alias", "alias2"},
		},
		{
			name: "creation networks take precedence and support multiple networks",
			options: containers.CreateContainerOptions{
				Network: "ignored",
				CreationNetworks: []containers.CreationNetwork{
					{Name: "net1", Aliases: []string{"db"}},
					{Name: "net2"},
				},
			},
			wantArgs: []string{"--network", "net1", "--network-alias", "db", "--network", "net2"},
		},
		{
			name:     "no network produces no args",
			options:  containers.CreateContainerOptions{},
			wantArgs: []string{},
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

func TestNormalizeCliErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stderr  string
		match   containers.ErrorMatch
		wantErr error
	}{
		{
			name:    "container not found",
			stderr:  "Container 'abc123' not found.",
			match:   newContainerNotFoundErrorMatch,
			wantErr: containers.ErrNotFound,
		},
		{
			name:    "network not found",
			stderr:  "Network not found: 'dcp-testnet'",
			match:   newNetworkNotFoundErrorMatch,
			wantErr: containers.ErrNotFound,
		},
		{
			name:    "volume not found",
			stderr:  "Volume not found: 'dcp-testvol'",
			match:   newVolumeNotFoundErrorMatch,
			wantErr: containers.ErrNotFound,
		},
		{
			name:    "image not found on inspect",
			stderr:  "Image 'doesnotexist123' not found.",
			match:   imageNotFoundErrorMatch,
			wantErr: containers.ErrNotFound,
		},
		{
			name:    "image not found on pull",
			stderr:  "pull access denied for doesnotexist123, repository does not exist\nError code: WSLC_E_IMAGE_NOT_FOUND",
			match:   imageNotFoundErrorMatch,
			wantErr: containers.ErrNotFound,
		},
		{
			name:    "network already exists",
			stderr:  "Cannot create a file when that file already exists. (ERROR_ALREADY_EXISTS)",
			match:   newNetworkAlreadyExistsErrorMatch,
			wantErr: containers.ErrAlreadyExists,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf := bytes.NewBufferString(tc.stderr)
			err := normalizeCliErrors(buf, tc.match)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestTarMissingDetection(t *testing.T) {
	t.Parallel()

	matching := []string{
		"exec: \"tar\": executable file not found in $PATH",
		"tar: not found",
		"sh: tar: command not found",
		"/bin/sh: tar: no such file or directory",
	}
	for _, s := range matching {
		require.True(t, tarMissingRegEx.MatchString(s), "expected tar-missing match for: %s", s)
	}

	nonMatching := []string{
		"Container 'abc123' not found.",
		"tar archive extracted successfully",
		"permission denied",
	}
	for _, s := range nonMatching {
		require.False(t, tarMissingRegEx.MatchString(s), "did not expect tar-missing match for: %s", s)
	}
}

func TestBuildCreateFilesError(t *testing.T) {
	t.Parallel()

	execErr := fmt.Errorf("exit status 1")

	t.Run("missing tar adds diagnostic hint", func(t *testing.T) {
		t.Parallel()
		err := buildCreateFilesError("web-1", "exec: \"tar\": executable file not found in $PATH", execErr)
		require.Error(t, err)
		require.ErrorIs(t, err, execErr)
		require.NotErrorIs(t, err, containers.ErrNotFound)
		require.Contains(t, err.Error(), "web-1")
		require.Contains(t, err.Error(), "`tar` binary on PATH")
	})

	t.Run("container not found is surfaced without tar hint", func(t *testing.T) {
		t.Parallel()
		err := buildCreateFilesError("web-1", "Container 'web-1' not found.", execErr)
		require.Error(t, err)
		require.ErrorIs(t, err, containers.ErrNotFound)
		require.NotContains(t, err.Error(), "`tar` binary on PATH")
	})

	t.Run("other failures do not get tar hint", func(t *testing.T) {
		t.Parallel()
		err := buildCreateFilesError("web-1", "permission denied", execErr)
		require.Error(t, err)
		require.ErrorIs(t, err, execErr)
		require.NotContains(t, err.Error(), "`tar` binary on PATH")
	})
}

func TestWriteImageLayerFile(t *testing.T) {
	t.Parallel()

	t.Run("writes base64 RawContents", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dest := filepath.Join(dir, "layer0.tar")
		payload := []byte("layer-contents")

		layer := &apiv1.ImageLayer{RawContents: base64.StdEncoding.EncodeToString(payload)}
		require.NoError(t, writeImageLayerFile(dest, layer))

		written, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, payload, written)
	})

	t.Run("copies from Source file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		source := filepath.Join(dir, "source.tar")
		payload := []byte("source-layer-contents")
		require.NoError(t, os.WriteFile(source, payload, 0600))

		dest := filepath.Join(dir, "layer0.tar")
		layer := &apiv1.ImageLayer{Source: source}
		require.NoError(t, writeImageLayerFile(dest, layer))

		written, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, payload, written)
	})

	t.Run("invalid base64 returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dest := filepath.Join(dir, "layer0.tar")

		layer := &apiv1.ImageLayer{RawContents: "not-valid-base64!!!"}
		require.Error(t, writeImageLayerFile(dest, layer))
	})

	t.Run("missing Source file returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dest := filepath.Join(dir, "layer0.tar")

		layer := &apiv1.ImageLayer{Source: filepath.Join(dir, "does-not-exist.tar")}
		require.Error(t, writeImageLayerFile(dest, layer))
	})
}

const inspectedRunningContainer = `
[
  {
    "Config": {
      "Cmd": null,
      "Entrypoint": ["dotnet", "/app/Aspire.Dashboard.dll"],
      "Env": ["PATH=/usr/local/sbin:/usr/local/bin", "APP_UID=1654"],
      "User": "1654",
      "WorkingDir": "/app"
    },
    "Created": "2026-06-30T10:00:53.872669951Z",
    "HostConfig": { "Memory": 0, "NanoCpus": 0, "NetworkMode": "bridge", "Ulimits": [] },
    "Id": "2187976cbf06c74c0582a4bd83bcfe50e6d7c268b753dc81130062868bb39320",
    "Image": "mcr.microsoft.com/dotnet/aspire-dashboard:latest",
    "Labels": {},
    "Mounts": [],
    "Name": "gleaming_scandinavian",
    "NetworkSettings": {
      "Networks": {
        "bridge": {
          "Aliases": [],
          "Gateway": "172.17.0.1",
          "IPAddress": "172.17.0.2",
          "IPPrefixLen": 16,
          "MacAddress": "02:42:ac:11:00:02"
        }
      }
    },
    "Ports": {
      "18888/tcp": [{ "HostIp": "127.0.0.1", "HostPort": "18888" }],
      "18889/tcp": [{ "HostIp": "127.0.0.1", "HostPort": "4317" }]
    },
    "State": {
      "ExitCode": 0,
      "FinishedAt": "0001-01-01T00:00:00Z",
      "Running": true,
      "StartedAt": "2026-06-30T10:00:54.32029315Z",
      "Status": "running"
    }
  }
]`

const inspectedExitedContainer = `
[
  {
    "Config": { "Cmd": null, "Entrypoint": ["dotnet"], "Env": [], "User": "", "WorkingDir": "/app" },
    "Created": "2026-06-30T10:00:53.872669951Z",
    "Id": "2187976cbf06c74c0582a4bd83bcfe50e6d7c268b753dc81130062868bb39320",
    "Image": "mcr.microsoft.com/dotnet/aspire-dashboard:latest",
    "Labels": {},
    "Mounts": [],
    "Name": "gleaming_scandinavian",
    "NetworkSettings": { "Networks": {} },
    "Ports": {},
    "State": {
      "ExitCode": 143,
      "FinishedAt": "2026-06-30T11:29:12.123456789Z",
      "Running": false,
      "StartedAt": "2026-06-30T10:00:54.32029315Z",
      "Status": "exited"
    }
  }
]`

const listedContainers = `
[
  { "CreatedAt": 1782813000, "Id": "aaa", "Image": "img:created", "Name": "created_one", "Ports": [], "State": 1, "StateChangedAt": 1782813001 },
  { "CreatedAt": 1782813653, "Id": "2187976cbf06c74c0582a4bd83bcfe50e6d7c268b753dc81130062868bb39320", "Image": "mcr.microsoft.com/dotnet/aspire-dashboard:latest", "Name": "gleaming_scandinavian", "Ports": [{ "BindingAddress": "127.0.0.1", "ContainerPort": 18888, "HostPort": 18888, "Protocol": 6 }], "State": 2, "StateChangedAt": 1782813654 },
  { "CreatedAt": 1782813100, "Id": "ccc", "Image": "img:exited", "Name": "exited_one", "Ports": [], "State": 3, "StateChangedAt": 1782813200 }
]`

const inspectedVolume = `
[
  { "CreatedAt": "2026-06-30T11:31:20Z", "Driver": "guest", "DriverOpts": {}, "Labels": {}, "Name": "dcp-testvol", "Status": null }
]`

const inspectedImage = `
[
  {
    "Architecture": "amd64",
    "Config": { "Cmd": null, "Entrypoint": ["dotnet"], "Env": [], "Labels": null },
    "Created": "2026-06-30T00:00:00Z",
    "Id": "sha256:a33c2e3a3c1f0b9e0e8c1f6f8b7a6d5c4b3a2918273645aef0123456789abcde",
    "Os": "linux",
    "RepoDigests": ["mcr.microsoft.com/dotnet/aspire-dashboard@sha256:1d584e3322bc9876543210fedcba0123456789abcdef0123456789abcdef0123"],
    "RepoTags": ["mcr.microsoft.com/dotnet/aspire-dashboard:latest"],
    "Size": 211401250
  }
]`

const inspectedNetwork = `
[
  {
    "Driver": "bridge",
    "IPAM": { "Config": [{ "Gateway": "172.18.0.1", "Subnet": "172.18.0.0/16" }], "Driver": "default" },
    "Id": "966ca82f0e0d1c2b3a4958677685949302010fedcba98765432100fedcba9876",
    "Internal": false,
    "Labels": { "com.microsoft.wsl.network.managed": "true" },
    "Name": "dcp-testnet",
    "Scope": "local"
  }
]`

const listedNetworks = `
[
  { "Driver": "bridge", "Id": "966ca82f0e0d1c2b3a4958677685949302010fedcba98765432100fedcba9876", "Name": "dcp-testnet" }
]`
