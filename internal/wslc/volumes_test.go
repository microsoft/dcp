/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

func TestVolumeLifecycleCommandsUseJsonAndPreserveRequestedNames(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "create", "--label", "owner=dcp", "volume-name"},
		"volume-name\n",
		"",
		0,
	)

	createErr := orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
		Name:   "volume-name",
		Labels: map[string]string{"owner": "dcp"},
	})
	require.NoError(t, createErr)

	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "inspect", "--format", "json", "volume-name"},
		`[{"Name":"volume-name","Driver":"guest","Labels":{"owner":"dcp"},"Mountpoint":"/var/lib/volume","Scope":"local","CreatedAt":"2026-01-02T03:04:05Z"}]`,
		"",
		0,
	)
	inspected, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{"volume-name"},
	})
	require.NoError(t, inspectErr)
	require.Len(t, inspected, 1)
	require.Equal(t, "guest", inspected[0].Driver)
	require.Equal(t, "dcp", inspected[0].Labels["owner"])
	require.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), inspected[0].CreatedAt)

	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "list", "--filter", "label=owner=dcp", "--format", "json"},
		`{"Name":"volume-name","Driver":"guest"}`+"\n",
		"",
		0,
	)
	listed, listErr := orchestrator.ListVolumes(ctx, containers.ListVolumesOptions{
		Filters: containers.ListVolumesFilters{
			LabelFilters: []containers.LabelFilter{{Key: "owner", Value: "dcp"}},
		},
	})
	require.NoError(t, listErr)
	require.Equal(t, []containers.ListedVolume{{Name: "volume-name"}}, listed)

	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "remove", "--force", "volume-name"},
		"volume-name\n",
		"",
		0,
	)
	removed, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{"volume-name"},
		Force:   true,
	})
	require.NoError(t, removeErr)
	require.Equal(t, []string{"volume-name"}, removed)
}

func TestInspectVolumesPreservesPartialSuccess(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "inspect", "--format", "json", "present", "missing"},
		`[{"Name":"present","Driver":"guest"}]`,
		"Volume not found: 'missing'\n",
		1,
	)

	inspected, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{"present", "missing"},
	})

	require.Equal(t, []containers.InspectedVolume{{Name: "present", Driver: "guest"}}, inspected)
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.ErrorIs(t, inspectErr, containers.ErrIncomplete)
}

func TestRemoveVolumesPreservesPartialSuccess(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(t, executor, []string{"wslc", "volume", "remove", "first"}, "first\n", "", 0)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "volume", "remove", "missing"},
		"",
		"Volume not found: 'missing'\n",
		1,
	)

	removed, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{"first", "missing"},
	})

	require.Equal(t, []string{"first"}, removed)
	require.True(t, errors.Is(removeErr, containers.ErrNotFound))
	require.True(t, errors.Is(removeErr, containers.ErrIncomplete))
}
