/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func TestApplyCreateContainerOptionsUsesNativeWslcSyntax(t *testing.T) {
	t.Parallel()

	args, applyErr := applyCreateContainerOptions([]string{"container", "create"}, containers.CreateContainerOptions{
		Name:       "test-container",
		Image:      "example.test/image:latest",
		Entrypoint: "/entrypoint",
		Command:    []string{"arg"},
		Env:        []containers.EnvVar{{Name: "ONE", Value: "two"}},
		EnvFiles:   []string{`C:\config path\env.list`},
		Ports: []containers.CreateContainerPort{{
			ContainerPort: 8080,
			Protocol:      "tcp",
		}},
		VolumeMounts: []containers.CreateContainerVolumeMount{{
			Type:     containers.BindMount,
			Source:   `C:\host path\data`,
			Target:   "/data",
			ReadOnly: true,
		}},
		Labels:     []containers.Label{{Key: "owner", Value: "dcp"}},
		PullPolicy: containers.PullPolicyMissing,
		Networks: []containers.CreateContainerNetworkOptions{
			{Name: "first", Aliases: []string{"one", "two"}},
			{Name: "second", Aliases: []string{"three"}},
		},
		Healthcheck: containers.ContainerHealthcheck{
			Command: []string{"CMD-SHELL", "test -f /ready"},
			Timeout: 2 * time.Second,
		},
		AttachTerminal: true,
		RunArgs:        []string{"--custom-option"},
	})

	require.NoError(t, applyErr)
	require.Equal(t, []string{
		"container", "create",
		"--name", "test-container",
		"--network", "name=first,alias=one,alias=two",
		"--network", "name=second,alias=three",
		"--mount", `type=bind,src=C:\host path\data,target=/data,readonly`,
		"--publish", "127.0.0.1::8080/tcp",
		"--env", "ONE=two",
		"--env-file", `C:\config path\env.list`,
		"--label", "owner=dcp",
		"--pull", "missing",
		"--entrypoint", "/entrypoint",
		"--health-cmd", "CMD-SHELL test -f /ready",
		"--health-interval", "30s",
		"--health-timeout", "2s",
		"--health-retries", "3",
		"--interactive", "--tty",
		"--custom-option",
	}, args)
}

func TestApplyCreateContainerOptionsRejectsUnsupportedSettings(t *testing.T) {
	t.Parallel()

	_, restartErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image:         "image",
		RestartPolicy: containers.RestartPolicyAlways,
	})
	require.ErrorContains(t, restartErr, "restart policy")

	_, startIntervalErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image: "image",
		Healthcheck: containers.ContainerHealthcheck{
			StartInterval: time.Second,
		},
	})
	require.ErrorContains(t, startIntervalErr, "start intervals")

	_, commandErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image: "image",
		Healthcheck: containers.ContainerHealthcheck{
			Timeout: time.Second,
		},
	})
	require.ErrorContains(t, commandErr, "require a health-check command")
}

func TestApplyCreateContainerOptionsDeduplicatesLabelsLastValueWins(t *testing.T) {
	t.Parallel()

	args, applyErr := applyCreateContainerOptions(
		[]string{"container", "create"},
		containers.CreateContainerOptions{
			Image:          "busybox:latest",
			Entrypoint:     "sh",
			AttachTerminal: true,
			Labels: []containers.Label{
				{Key: "persistent", Value: "tracker"},
				{Key: "owner", Value: "dcp"},
				{Key: "creator", Value: "first"},
				{Key: "persistent", Value: "controller"},
				{Key: "creator", Value: "last"},
			},
		},
	)

	require.NoError(t, applyErr)
	require.Equal(t, []string{
		"container", "create",
		"--label", "creator=last",
		"--label", "owner=dcp",
		"--label", "persistent=controller",
		"--entrypoint", "sh",
		"--interactive", "--tty",
	}, args)
}

func TestCreateContainerResolvesInitialNetworkIDWithoutAliases(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "full-network-id"},
		`[{"Id":"full-network-id","Name":"network-name","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "create", "--network", "network-name", "image", "command"},
		"container-id\n",
		"",
		0,
	)
	requestedNetworks := []containers.CreateContainerNetworkOptions{{
		Name: "full-network-id",
	}}

	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Image:    "image",
		Command:  []string{"command"},
		Networks: requestedNetworks,
	})

	require.NoError(t, createErr)
	require.Equal(t, "container-id", containerID)
	require.Equal(t, []containers.CreateContainerNetworkOptions{{Name: "full-network-id"}}, requestedNetworks)
}

func TestRunContainerResolvesMultipleInitialNetworkIDsAndPreservesAliases(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "first-network-id"},
		`[{"Id":"first-network-id","Name":"first-network","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "second-network-id"},
		`[{"Id":"second-network-id","Name":"second-network","IPAM":{"Config":[]},"Containers":{}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{
			"wslc", "container", "run",
			"--network", "name=first-network,alias=first-alias",
			"--network", "name=second-network,alias=second-alias,alias=extra-alias",
			"--detach",
			"image", "command",
		},
		"container-id\n",
		"",
		0,
	)
	requestedNetworks := []containers.CreateContainerNetworkOptions{
		{Name: "first-network-id", Aliases: []string{"first-alias"}},
		{Name: "second-network-id", Aliases: []string{"second-alias", "extra-alias"}},
	}
	expectedRequestedNetworks := []containers.CreateContainerNetworkOptions{
		{Name: "first-network-id", Aliases: []string{"first-alias"}},
		{Name: "second-network-id", Aliases: []string{"second-alias", "extra-alias"}},
	}

	containerID, runErr := orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Image:    "image",
			Command:  []string{"command"},
			Networks: requestedNetworks,
		},
	})

	require.NoError(t, runErr)
	require.Equal(t, "container-id", containerID)
	require.Equal(t, expectedRequestedNetworks, requestedNetworks)
}

func TestContainerCreationReturnsInitialNetworkResolutionErrors(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"create", "run"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			ctx, orchestrator, executor := newTestOrchestrator(t)
			installAutoCommand(
				t,
				executor,
				[]string{"wslc", "network", "inspect", "--format", "json", "missing-network-id"},
				`[]`,
				"Network not found: 'missing-network-id'\n",
				1,
			)
			options := containers.CreateContainerOptions{
				Image: "image",
				Networks: []containers.CreateContainerNetworkOptions{{
					Name:    "missing-network-id",
					Aliases: []string{"alias"},
				}},
			}

			var creationErr error
			if operation == "create" {
				_, creationErr = orchestrator.CreateContainer(ctx, options)
			} else {
				_, creationErr = orchestrator.RunContainer(ctx, containers.RunContainerOptions{
					CreateContainerOptions: options,
				})
			}

			require.ErrorIs(t, creationErr, containers.ErrNotFound)
			require.ErrorContains(t, creationErr, `resolving initial container network "missing-network-id"`)
			require.Empty(t, executor.FindAll([]string{"wslc", "container", operation}, "", nil))
		})
	}
}

func TestInspectContainersMapsWslcLayoutAndResolvesNetworkIDs(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container-name"},
		`[{
			"Id":"container-id",
			"Name":"/container-name",
			"Created":"2026-01-02T03:04:05Z",
			"Config":{
				"Image":"example.test/image:tag",
				"Cmd":["arg1","arg2"],
				"Entrypoint":["/entrypoint"],
				"Env":["ONE=two","EMPTY","WITH_EQUALS=a=b"],
				"Labels":{"owner":"dcp"},
				"Healthcheck":{"Test":["CMD-SHELL","test -f /ready"]}
			},
			"State":{
				"Status":"running",
				"Running":true,
				"StartedAt":"2026-01-02T03:05:05Z",
				"FinishedAt":"0001-01-01T00:00:00Z",
				"ExitCode":0,
				"Error":"",
				"Health":{"Status":"healthy","FailingStreak":0,"Log":[]}
			},
			"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},
			"Mounts":[
				{"Type":"bind","Source":"C:\\host path\\data","Destination":"/bind","ReadWrite":false},
				{"Type":"volume","Source":"/ignored","Name":"named-volume","Destination":"/volume","ReadWrite":true}
			],
			"NetworkSettings":{"Networks":{
				"bridge":{"Aliases":["container-name"],"Gateway":"172.20.0.1","IPAddress":"172.20.0.2","MacAddress":"00:11:22:33:44:55"},
				"custom":{"Aliases":["alias"],"Gateway":"172.21.0.1","IPAddress":"172.21.0.2","MacAddress":"00:11:22:33:44:66"}
			}}
		}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "bridge", "custom"},
		`[
			{"Id":"bridge-id","Name":"bridge","Driver":"bridge","IPAM":{"Config":[]},"Containers":{}},
			{"Id":"custom-id","Name":"custom","Driver":"bridge","IPAM":{"Config":[]},"Containers":{}}
		]`,
		"",
		0,
	)

	inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{"container-name"},
	})

	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	container := inspected[0]
	require.Equal(t, "container-id", container.Id)
	require.Equal(t, "container-name", container.Name)
	require.Equal(t, "example.test/image:tag", container.Image)
	require.Equal(t, containers.ContainerStatusRunning, container.Status)
	require.Equal(t, []string{"/entrypoint", "arg1", "arg2"}, container.Args)
	require.Equal(t, "two", container.Env["ONE"])
	require.Equal(t, "", container.Env["EMPTY"])
	require.Equal(t, "a=b", container.Env["WITH_EQUALS"])
	require.Equal(t, []string{"CMD-SHELL", "test -f /ready"}, container.Healthcheck)
	require.NotNil(t, container.Health)
	require.Equal(t, "healthy", container.Health.Status)
	require.Equal(t, "49152", container.Ports["8080/tcp"][0].HostPort)
	require.Equal(t, containers.VolumeMount{
		Type:     containers.BindMount,
		Source:   `C:\host path\data`,
		Target:   "/bind",
		ReadOnly: true,
	}, container.Mounts[0])
	require.Equal(t, "named-volume", container.Mounts[1].Source)
	require.Equal(t, "bridge-id", container.Networks[0].Id)
	require.Equal(t, "custom-id", container.Networks[1].Id)
	require.Equal(t, "alias", container.Networks[1].Aliases[0])
	require.Equal(t, "dcp", container.Labels["owner"])
}

func TestInspectContainersDoesNotInventMissingNetworkIDs(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container-name"},
		`[{"Id":"container-id","Name":"/container-name","Config":{"Image":"image"},"State":{"Status":"created"},"NetworkSettings":{"Networks":{"gone":{}}}}]`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "gone"},
		`[]`,
		"Network not found: 'gone'\n",
		1,
	)

	inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{"container-name"},
	})

	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.Len(t, inspected, 1)
	require.Len(t, inspected[0].Networks, 1)
	require.Equal(t, "gone", inspected[0].Networks[0].Name)
	require.Empty(t, inspected[0].Networks[0].Id)
}

func TestInspectContainersPreservesValidObjectAlongsideMissingReference(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "present", "missing"},
		`[{"Id":"container-id","Name":"/present","Config":{"Image":"image"},"State":{"Status":"running"},"NetworkSettings":{"Networks":{"bridge":{}}}}]`,
		"Container 'missing' not found.\n",
		1,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "network", "inspect", "--format", "json", "bridge"},
		`[{"Id":"bridge-id","Name":"bridge","IPAM":{"Config":[]},"Containers":{"container-id":{"Name":"present"}}}]`,
		"",
		0,
	)

	inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{"present", "missing"},
	})

	require.Len(t, inspected, 1)
	require.Equal(t, "container-id", inspected[0].Id)
	require.Equal(t, "bridge-id", inspected[0].Networks[0].Id)
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.ErrorIs(t, inspectErr, containers.ErrIncomplete)
}

func TestListContainersUsesInspectionForLabelsWithCommas(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "list", "--no-trunc", "--all", "--filter", "label=owner=dcp", "--format", "json"},
		`{"ID":"container-id","Names":"container-name","Image":"image","State":"running","Networks":"bridge, custom","Labels":"owner=dcp,com.microsoft.wslc.metadata={\"one\":1,\"two\":2}"}`+"\n",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "inspect", "--format", "json", "container-id"},
		`[{"Id":"container-id","Name":"/container-name","Config":{"Labels":{"owner":"dcp","value":"one,two=three"}}}]`,
		"",
		0,
	)

	listed, listErr := orchestrator.ListContainers(ctx, containers.ListContainersOptions{
		All: true,
		Filters: containers.ListContainersFilters{
			LabelFilters: []containers.LabelFilter{{Key: "owner", Value: "dcp"}},
		},
	})

	require.NoError(t, listErr)
	require.Len(t, listed, 1)
	require.Equal(t, "one,two=three", listed[0].Labels["value"])
	require.Equal(t, []string{"bridge", "custom"}, listed[0].Networks)
}

func TestListContainersResolvesNetworkIDFiltersToNativeNames(t *testing.T) {
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
		[]string{"wslc", "container", "list", "--no-trunc", "--all", "--filter", "network=network-name", "--format", "json"},
		"",
		"",
		0,
	)

	listed, listErr := orchestrator.ListContainers(ctx, containers.ListContainersOptions{
		All: true,
		Filters: containers.ListContainersFilters{
			NetworkFilters: []string{"network-id"},
		},
	})

	require.NoError(t, listErr)
	require.Empty(t, listed)
	require.Empty(t, executor.FindAll(
		[]string{"wslc", "container", "list", "--no-trunc", "--all", "--filter", "network=network-id"},
		"",
		nil,
	))
}

func TestCreateContainerReturnsPartialIDOnCommandFailure(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "create", "--name", "container-name", "image"},
		"container-id\n",
		"unexpected post-create failure\n",
		1,
	)

	containerID, createErr := orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:  "container-name",
		Image: "image",
	})

	require.Equal(t, "container-id", containerID)
	require.Error(t, createErr)
}

func TestRunContainerUsesDetachAndReturnsID(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{
			"wslc", "container", "run",
			"--name", "container-name",
			"--detach",
			"--custom-option",
			"image",
			"command", "arg",
		},
		"container-id\n",
		"",
		0,
	)

	containerID, runErr := orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    "container-name",
			Image:   "image",
			Command: []string{"command", "arg"},
			RunArgs: []string{"--custom-option"},
		},
	})

	require.NoError(t, runErr)
	require.Equal(t, "container-id", containerID)
}

func TestStartContainersRunsOneNativeCommandPerContainer(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(t, executor, []string{"wslc", "container", "start", "first"}, "first\n", "", 0)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "start", "missing"},
		"",
		"Container 'missing' not found.\n",
		1,
	)
	installAutoCommand(t, executor, []string{"wslc", "container", "start", "third"}, "third\n", "", 0)

	started, startErr := orchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{"first", "missing", "third"},
	})

	require.Equal(t, []string{"first", "third"}, started)
	require.ErrorIs(t, startErr, containers.ErrNotFound)
	require.ErrorIs(t, startErr, containers.ErrIncomplete)
	require.Len(t, executor.FindAll([]string{"wslc", "container", "start"}, "", nil), 3)
}

func TestStopContainersUsesNativeTimeoutFlag(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "stop", "-t", "5", "container"},
		"container\n",
		"",
		0,
	)

	stopped, stopErr := orchestrator.StopContainers(ctx, containers.StopContainersOptions{
		Containers:    []string{"container"},
		SecondsToKill: 5,
	})

	require.NoError(t, stopErr)
	require.Equal(t, []string{"container"}, stopped)
}

func TestExecContainerKeepsStreamsSeparateAndBuffersExitCode(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "exec", "--workdir", "/work", "--env", "ONE=two", "container", "command", "arg"},
		"stdout-value",
		"stderr-value",
		7,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCodes, execErr := orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
		Container:        "container",
		WorkingDirectory: "/work",
		Env:              []containers.EnvVar{{Name: "ONE", Value: "two"}},
		Command:          "command",
		Args:             []string{"arg"},
		StreamCommandOptions: containers.StreamCommandOptions{
			StdOutStream: usvc_io.NopWriteCloser(&stdout),
			StdErrStream: usvc_io.NopWriteCloser(&stderr),
		},
	})

	require.NoError(t, execErr)
	require.Equal(t, 1, cap(exitCodes))
	require.Equal(t, int32(7), <-exitCodes)
	_, open := <-exitCodes
	require.False(t, open)
	require.Equal(t, "stdout-value", stdout.String())
	require.Equal(t, "stderr-value", stderr.String())
}

func TestCreateFilesCopiesGeneratedArchiveOnStdin(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	var archiveSize int
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"wslc", "container", "cp", "-a=false", "-", "container:/"},
		},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			archive, readErr := io.ReadAll(execution.Cmd.Stdin)
			require.NoError(t, readErr)
			archiveSize = len(archive)
			return 0
		},
	})

	createErr := orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
		Container:   "container",
		Destination: "/data",
		ModTime:     time.Unix(1, 0),
		Entries: []containers.FileSystemEntry{{
			Name:     "file.txt",
			Contents: "contents",
		}},
	})

	require.NoError(t, createErr)
	require.Positive(t, archiveSize)
}

func TestCreateFilesRejectsEmptyEntries(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	createErr := orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
		Container: "container",
	})

	require.ErrorContains(t, createErr, "at least one file-system entry")
	require.Empty(t, executor.Executions)
}

func TestCaptureContainerLogsSeparatesAndClosesStreams(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "logs", "--follow", "--timestamps", "container"},
		"stdout-log",
		"stderr-log",
		0,
	)
	stdout := newTestWriteSyncCloser()
	stderr := newTestWriteSyncCloser()

	captureErr := orchestrator.CaptureContainerLogs(
		ctx,
		"container",
		stdout,
		stderr,
		containers.StreamContainerLogsOptions{Follow: true, Timestamps: true},
	)
	require.NoError(t, captureErr)

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-stdout.closed:
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-stderr.closed:
	}
	require.Equal(t, "stdout-log", stdout.String())
	require.Equal(t, "stderr-log", stderr.String())
	require.Equal(t, int32(0), stdout.syncCount.Load())
	require.Equal(t, int32(0), stderr.syncCount.Load())
}

func TestSequentialOperationsPreservePartialResults(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(t, executor, []string{"wslc", "container", "remove", "--volumes", "--force", "first"}, "", "", 0)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "container", "remove", "--volumes", "--force", "missing"},
		"",
		"Container 'missing' not found.\n",
		1,
	)

	removed, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{"first", "missing"},
		Force:      true,
	})

	require.Equal(t, []string{"first"}, removed)
	require.True(t, errors.Is(removeErr, containers.ErrNotFound))
	require.True(t, errors.Is(removeErr, containers.ErrIncomplete))
}
