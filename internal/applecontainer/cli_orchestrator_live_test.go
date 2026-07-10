/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const liveTestImage = "docker.io/library/alpine:latest"

// TestLiveAppleContainerOrchestrator exercises the orchestrator against a real Apple
// `container` CLI installation. It is skipped unless DCP_TEST_APPLE_CONTAINER=1 is set
// (requires macOS with the Apple container runtime installed and running).
func TestLiveAppleContainerOrchestrator(t *testing.T) {
	if os.Getenv("DCP_TEST_APPLE_CONTAINER") != "1" {
		t.Skip("Set DCP_TEST_APPLE_CONTAINER=1 to run tests against a live Apple container runtime")
	}

	ctx, cancel := testutil.GetTestContext(t, 3*time.Minute)
	defer cancel()

	log := testr.New(t)
	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	aco := NewAppleContainerCliOrchestrator(log, executor)

	status := aco.CheckStatus(ctx, containers.IgnoreCachedRuntimeStatus)
	require.True(t, status.IsHealthy(), "The Apple container runtime must be installed and running for live tests: %s", status.Error)

	containerName := fmt.Sprintf("dcp-live-test-%d", time.Now().UnixNano())
	volumeName := containerName + "-vol"

	// Volume lifecycle
	require.NoError(t, aco.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName}))
	defer func() {
		_, _ = aco.RemoveVolumes(ctx, containers.RemoveVolumesOptions{Volumes: []string{volumeName}})
	}()

	volumes, err := aco.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	require.Equal(t, volumeName, volumes[0].Name)

	// Image pull + inspect
	imageId, err := aco.PullImage(ctx, containers.PullImageOptions{Image: liveTestImage})
	require.NoError(t, err)
	require.NotEmpty(t, imageId)

	images, err := aco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{liveTestImage}})
	require.NoError(t, err)
	require.Len(t, images, 1)
	require.Equal(t, imageId, images[0].Id)

	// The first container triggers DNS forwarder provisioning; make sure the forwarder
	// and its records directory are cleaned up after the test (registered before the
	// container cleanups below so it runs last).
	defer func() {
		_, _ = aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{dnsForwarderContainerName()}, Force: true})
		_ = os.RemoveAll(dnsRecordsHostDir())
	}()

	// Run a container with a label, a published port, and a volume mount
	containerId, err := aco.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:     containerName,
			Networks: []containers.CreateContainerNetworkOptions{{Name: aco.DefaultNetworkName()}},
			ContainerSpec: apiv1.ContainerSpec{
				Image: liveTestImage,
				Labels: []apiv1.ContainerLabel{
					{Key: "usvc-dev.dcp/live-test", Value: containerName},
				},
				Ports: []apiv1.ContainerPort{
					{ContainerPort: 80},
				},
				VolumeMounts: []apiv1.VolumeMount{
					{Type: apiv1.NamedVolumeMount, Source: volumeName, Target: "/data"},
				},
				Command: "sleep",
				Args:    []string{"120"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, containerName, containerId)
	defer func() {
		_, _ = aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{containerName}, Force: true})
	}()

	// The container should be listed, and label filtering should work
	listed, err := aco.ListContainers(ctx, containers.ListContainersOptions{
		Filters: containers.ListContainersFilters{
			LabelFilters: []containers.LabelFilter{{Key: "usvc-dev.dcp/live-test", Value: containerName}},
		},
	})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, containerName, listed[0].Id)
	require.Equal(t, containers.ContainerStatusRunning, listed[0].Status)

	// Inspect: status, ports (auto-allocated host port), networks, mounts
	inspected, err := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerName}})
	require.NoError(t, err)
	require.Len(t, inspected, 1)
	require.Equal(t, containers.ContainerStatusRunning, inspected[0].Status)
	require.Len(t, inspected[0].Ports["80/tcp"], 1)
	require.NotEqual(t, "0", inspected[0].Ports["80/tcp"][0].HostPort)
	require.Len(t, inspected[0].Networks, 1)
	require.Equal(t, defaultNetworkName, inspected[0].Networks[0].Name)
	require.NotEmpty(t, inspected[0].Networks[0].IPAddress)
	require.NotEmpty(t, inspected[0].Networks[0].Gateway)

	// ConnectNetwork reports the container as already attached to the default network
	connectErr := aco.ConnectNetwork(ctx, containers.ConnectNetworkOptions{Network: defaultNetworkName, Container: containerName})
	require.ErrorIs(t, connectErr, containers.ErrAlreadyExists)

	// DisconnectNetwork is a no-op
	require.NoError(t, aco.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{Network: defaultNetworkName, Container: containerName, Force: true}))

	// The default network inspection reports the attached container
	networks, err := aco.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{defaultNetworkName}})
	require.NoError(t, err)
	require.Len(t, networks, 1)
	require.NotEmpty(t, networks[0].Gateways)
	require.Contains(t, networks[0].Containers, containers.InspectedNetworkContainer{Id: containerName, Name: containerName})

	// CreateNetwork maps onto the default network
	networkId, err := aco.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "dcp-live-test-net"})
	require.NoError(t, err)
	require.Equal(t, defaultNetworkName, networkId)

	// CreateFiles copies files into the running container via exec+tar
	require.NoError(t, aco.CreateFiles(ctx, containers.CreateFilesOptions{
		Container:   containerName,
		ModTime:     time.Now(),
		Destination: "/dcp-test-files",
		Entries: []apiv1.FileSystemEntry{
			{Type: apiv1.FileSystemEntryTypeFile, Name: "hello.txt", Contents: "hello from DCP"},
		},
	}))

	// Exec: verify the created file content and the exit code
	var execOut bytes.Buffer
	exitCh, err := aco.ExecContainer(ctx, containers.ExecContainerOptions{
		Container: containerName,
		Command:   "cat",
		Args:      []string{"/dcp-test-files/hello.txt"},
		StreamCommandOptions: containers.StreamCommandOptions{
			StdOutStream: usvc_io.NopWriteCloser(&execOut),
		},
	})
	require.NoError(t, err)
	select {
	case exitCode := <-exitCh:
		require.EqualValues(t, 0, exitCode)
	case <-ctx.Done():
		t.Fatal("timed out waiting for exec to complete")
	}
	require.Equal(t, "hello from DCP", execOut.String())

	// Logs: the container has no output, but the log capture must start cleanly
	require.NoError(t, aco.CaptureContainerLogs(ctx, containerName, &nopWriteSyncerCloser{}, &nopWriteSyncerCloser{}, containers.StreamContainerLogsOptions{}))

	// ApplyImageLayers: bake an in-memory tar layer into a derived image via the real builder.
	// This is the mechanism the container controller uses to inject create-files and certificates
	// on this runtime (see CreateFilesRequiresRunningContainer), so it must work end to end.
	layerTar, tarErr := containers.BuildCreateFilesTar(containers.CreateFilesOptions{
		Destination: "/dcp-baked",
		Entries: []apiv1.FileSystemEntry{
			{Type: apiv1.FileSystemEntryTypeFile, Name: "baked.txt", Contents: "baked by DCP"},
		},
		ModTime: time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
	}, log)
	require.NoError(t, tarErr)
	require.NotNil(t, layerTar)

	layerDigest := sha256.Sum256(layerTar)
	derivedTag := fmt.Sprintf("dcp-live-test-derived:%d", time.Now().UnixNano())
	derivedImage, applyErr := aco.ApplyImageLayers(ctx, containers.ApplyImageLayersOptions{
		BaseImage: images[0],
		Layers: []apiv1.ImageLayer{{
			Digest:      hex.EncodeToString(layerDigest[:]),
			RawContents: base64.StdEncoding.EncodeToString(layerTar),
		}},
		Tag: derivedTag,
	})
	require.NoError(t, applyErr)
	require.Equal(t, derivedTag, derivedImage)
	runner := aco.(containers.CLICommandRunner)
	defer func() {
		rmCmd := makeAppleContainerCommand("image", "rm", derivedTag)
		_, _, _ = runner.RunBufferedCommand(ctx, "RemoveImage", rmCmd, nil, nil, ordinaryCommandTimeout)
	}()

	// The derived image must be inspectable (this is what the controller's cache probe does)
	// and must contain the baked file.
	derivedInspected, derivedInspectErr := aco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{derivedTag}})
	require.NoError(t, derivedInspectErr)
	require.Len(t, derivedInspected, 1)

	bakedContainerName := containerName + "-baked"
	var bakedOut bytes.Buffer
	_, bakedRunErr := aco.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name: bakedContainerName,
			ContainerSpec: apiv1.ContainerSpec{
				Image:   derivedTag,
				Command: "cat",
				Args:    []string{"/dcp-baked/baked.txt"},
			},
		},
	})
	require.NoError(t, bakedRunErr)
	defer func() {
		_, _ = aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{bakedContainerName}, Force: true})
	}()

	// The baked container prints the injected file and exits; read its logs to confirm the content.
	require.Eventually(t, func() bool {
		logsCmd := makeAppleContainerCommand("logs", bakedContainerName)
		logsOut, _, logsErr := runner.RunBufferedCommand(ctx, "Logs", logsCmd, nil, nil, ordinaryCommandTimeout)
		if logsErr != nil {
			return false
		}
		bakedOut.Reset()
		bakedOut.Write(logsOut.Bytes())
		return strings.Contains(bakedOut.String(), "baked by DCP")
	}, 60*time.Second, 2*time.Second, "the derived image should contain the baked file (logs: %s)", bakedOut.String())

	// DNS forwarder: every container is pointed at the DCP DNS forwarder, so network
	// aliases and container names resolve between containers. Run a second container with
	// an alias and resolve it from the first one.
	aliasedContainerName := containerName + "-aliased"
	_, aliasedRunErr := aco.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name: aliasedContainerName,
			Networks: []containers.CreateContainerNetworkOptions{
				{Name: aco.DefaultNetworkName(), Aliases: []string{"dcp-live-alias"}},
			},
			ContainerSpec: apiv1.ContainerSpec{
				Image:   liveTestImage,
				Command: "sleep",
				Args:    []string{"60"},
			},
		},
	})
	require.NoError(t, aliasedRunErr)
	defer func() {
		_, _ = aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{aliasedContainerName}, Force: true})
	}()

	aliasedInspected, aliasedInspectErr := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{aliasedContainerName}})
	require.NoError(t, aliasedInspectErr)
	require.Len(t, aliasedInspected, 1)
	aliasedIP := aliasedInspected[0].Networks[0].IPAddress
	require.NotEmpty(t, aliasedIP)

	// Helper: run a shell command in the first container and return its output.
	execInFirstContainer := func(script string) string {
		var out bytes.Buffer
		execExitCh, execErr := aco.ExecContainer(ctx, containers.ExecContainerOptions{
			Container: containerName,
			Command:   "sh",
			Args:      []string{"-c", script},
			StreamCommandOptions: containers.StreamCommandOptions{
				StdOutStream: usvc_io.NopWriteCloser(&out),
			},
		})
		require.NoError(t, execErr)
		select {
		case <-execExitCh:
		case <-ctx.Done():
			t.Fatal("timed out waiting for exec to complete")
		}
		return out.String()
	}

	// The first container (also pointed at the forwarder) must resolve the alias, the
	// container name, and still resolve external names through the upstream relay.
	// Record propagation is eventually consistent (the forwarder observes the records
	// file through a shared filesystem), hence the retries. Note: the first container was
	// created before the aliased one, proving records propagate to running containers.
	var dnsOut string
	require.Eventually(t, func() bool {
		dnsOut = execInFirstContainer(
			"nslookup dcp-live-alias 2>&1 | tail -2; nslookup " + aliasedContainerName + " > /dev/null 2>&1 && echo name-ok; nslookup example.com > /dev/null 2>&1 && echo upstream-ok")
		return strings.Contains(dnsOut, aliasedIP) && strings.Contains(dnsOut, "name-ok") && strings.Contains(dnsOut, "upstream-ok")
	}, 30*time.Second, 2*time.Second,
		"the alias should resolve to the aliased container's IP, the container name should resolve, and external names should resolve via the upstream relay (last output: %s)", &dnsOut)

	// Removing the aliased container must retract its records.
	_, aliasedRemoveErr := aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{aliasedContainerName}, Force: true})
	require.NoError(t, aliasedRemoveErr)

	var dnsGoneOut string
	require.Eventually(t, func() bool {
		dnsGoneOut = execInFirstContainer("nslookup dcp-live-alias > /dev/null 2>&1 || echo alias-gone")
		return strings.Contains(dnsGoneOut, "alias-gone")
	}, 30*time.Second, 2*time.Second,
		"records of removed containers should be retracted (last output: %s)", &dnsGoneOut)

	// Stop and remove
	stopped, err := aco.StopContainers(ctx, containers.StopContainersOptions{Containers: []string{containerName}, SecondsToKill: 5})
	require.NoError(t, err)
	require.Equal(t, []string{containerName}, stopped)

	inspected, err = aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerName}})
	require.NoError(t, err)
	require.Len(t, inspected, 1)
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status)

	removed, err := aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{containerName}})
	require.NoError(t, err)
	require.Equal(t, []string{containerName}, removed)

	// Volume cleanup must succeed now that the container is gone
	removedVolumes, err := aco.RemoveVolumes(ctx, containers.RemoveVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, err)
	require.Equal(t, []string{volumeName}, removedVolumes)
}

type nopWriteSyncerCloser struct {
	strings.Builder
}

func (n *nopWriteSyncerCloser) Sync() error  { return nil }
func (n *nopWriteSyncerCloser) Close() error { return nil }
