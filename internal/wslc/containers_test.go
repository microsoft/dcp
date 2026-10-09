/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

// Verifies native CLI encoding of container networks, mounts, ports, environment, labels, health checks, and terminal options.
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

// Verifies rejection of unsupported restart policies and health start intervals, and health settings without a command.
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

// Verifies that duplicate label keys produce one native argument per key using the last supplied value.
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
