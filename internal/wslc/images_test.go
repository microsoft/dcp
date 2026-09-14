/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestBuildImageUsesDirectoryContextPlainProgressAndIIDFile(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	iidFile := filepath.Join(t.TempDir(), "image.iid")
	validImageID := "sha256:" + strings.Repeat("a", 64)
	contextPath := `C:\build context`
	dockerfilePath := `C:\build context\Containerfile`
	expectedCommand := []string{
		"wslc", "image", "build",
		"--file", dockerfilePath,
		"--pull",
		"--iidfile", iidFile,
		"--tag", "example.test/image:tag",
		"--build-arg", "ARG=value",
		"--secret", `id=file-secret,type=file,src=C:\secrets\file`,
		"--secret", "id=env-secret,type=env,env=SECRET_ENV",
		"--target", "final",
		"--label", "owner=dcp",
		"--progress", "plain",
		contextPath,
	}
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: expectedCommand},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			require.NotContains(t, strings.Join(execution.Cmd.Args, " "), "secret-value")
			require.Contains(t, execution.Cmd.Environ(), "SECRET_ENV=secret-value")
			writeErr := usvc_io.WriteFile(iidFile, []byte(validImageID+"\n"), osutil.PermissionOnlyOwnerReadWrite)
			require.NoError(t, writeErr)
			_, stderrErr := execution.Cmd.Stderr.Write([]byte("build progress\n"))
			require.NoError(t, stderrErr)
			return 0
		},
	})

	buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		IidFile: iidFile,
		Pull:    true,
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context:    contextPath,
			Dockerfile: dockerfilePath,
			Tags:       []string{"example.test/image:tag"},
			Args:       []containers.EnvVar{{Name: "ARG", Value: "value"}},
			Secrets: []containers.ContainerBuildSecret{
				{ID: "file-secret", Type: containers.FileSecret, Source: `C:\secrets\file`},
				{ID: "env-secret", Type: containers.EnvSecret, Source: "SECRET_ENV", Value: "secret-value"},
			},
			Stage: "final",
			Labels: []containers.Label{
				{Key: "owner", Value: "old"},
				{Key: "owner", Value: "dcp"},
			},
		},
	})

	require.NoError(t, buildErr)
	require.Len(t, executor.FindAll(expectedCommand, "", nil), 1)
}

func TestBuildImageRejectsUnsupportedPlatformAndMissingIID(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	platformErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context:  `C:\context`,
			Platform: "linux/amd64",
		},
	})
	require.ErrorContains(t, platformErr, "does not support selecting build platform")
	require.Empty(t, executor.Executions)

	iidFile := filepath.Join(t.TempDir(), "missing.iid")
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "build", "--iidfile", iidFile, "--progress", "plain", `C:\context`},
		"",
		"",
		0,
	)
	iidErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		IidFile: iidFile,
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context: `C:\context`,
		},
	})
	require.ErrorContains(t, iidErr, "inspecting image ID file")
}

func TestBuildImageRejectsInvalidIIDFileResults(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		contents      string
		nonRegular    bool
		errorContains string
	}{
		{
			name:          "short digest",
			contents:      "sha256:" + strings.Repeat("a", 63),
			errorContains: "expected sha256: followed by 64 hexadecimal characters",
		},
		{
			name:          "nonhex digest",
			contents:      "sha256:" + strings.Repeat("a", 63) + "g",
			errorContains: "decoding SHA256 value",
		},
		{
			name:          "oversized output",
			contents:      strings.Repeat("a", 1025),
			errorContains: "exceeds 1024 bytes",
		},
		{
			name:          "nonregular output",
			nonRegular:    true,
			errorContains: "is not a regular file",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, orchestrator, executor := newTestOrchestrator(t)
			iidPath := filepath.Join(t.TempDir(), "image.iid")
			if testCase.nonRegular {
				iidPath = t.TempDir()
			}

			expectedCommand := []string{
				"wslc", "image", "build",
				"--iidfile", iidPath,
				"--progress", "plain",
				`C:\context`,
			}
			executor.InstallAutoExecution(internal_testutil.AutoExecution{
				Condition: internal_testutil.ProcessSearchCriteria{Command: expectedCommand},
				RunCommand: func(*internal_testutil.ProcessExecution) int32 {
					if testCase.nonRegular {
						return 0
					}
					writeErr := usvc_io.WriteFile(
						iidPath,
						[]byte(testCase.contents),
						osutil.PermissionOnlyOwnerReadWrite,
					)
					require.NoError(t, writeErr)
					return 0
				},
			})

			buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
				IidFile: iidPath,
				ContainerBuildContext: &containers.ContainerBuildContext{
					Context: `C:\context`,
				},
			})

			require.ErrorContains(t, buildErr, testCase.errorContains)
		})
	}
}

func TestInspectImagesPreservesPartialResultsFromFailedCommand(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "inspect", "--format", "json", "present", "missing"},
		`[{"Id":"sha256:image-id","RepoTags":["present:latest"],"RepoDigests":["present@sha256:digest"],"Config":{"Labels":{"owner":"dcp"}}}]`,
		"Image 'missing' not found.\n",
		1,
	)

	inspected, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{"present", "missing"},
	})

	require.Len(t, inspected, 1)
	require.Equal(t, "sha256:image-id", inspected[0].Id)
	require.Equal(t, "sha256:digest", inspected[0].Digest)
	require.Equal(t, "dcp", inspected[0].Labels["owner"])
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
	require.ErrorIs(t, inspectErr, containers.ErrIncomplete)
}

func TestPullImageFallsBackToInspectionWhenQuietOutputIsEmpty(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "pull", "--quiet", "example.test/image:tag"},
		"",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "inspect", "--format", "json", "example.test/image:tag"},
		`[{"Id":"sha256:pulled-image","RepoTags":["example.test/image:tag"],"Config":{}}]`,
		"",
		0,
	)

	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{
		Image: "example.test/image:tag",
	})

	require.NoError(t, pullErr)
	require.Equal(t, "sha256:pulled-image", imageID)
}

func TestRemoveImagesReturnsRequestedReferencesWithPartialErrors(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "remove", "--force", "first:latest"},
		"Deleted: sha256:first\n",
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "image", "remove", "--force", "missing:latest"},
		"",
		"Image 'missing:latest' not found.\n",
		1,
	)

	removed, removeErr := orchestrator.RemoveImages(ctx, containers.RemoveImagesOptions{
		Images: []string{"first:latest", "missing:latest"},
		Force:  true,
	})

	require.Equal(t, []string{"first:latest"}, removed)
	require.True(t, errors.Is(removeErr, containers.ErrNotFound))
	require.True(t, errors.Is(removeErr, containers.ErrIncomplete))
}

func TestApplyImageLayersUsesDiskBackedBuildContext(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"wslc", "image", "build", "--file"},
			Cond: func(execution *internal_testutil.ProcessExecution) bool {
				return strings.Contains(strings.Join(execution.Cmd.Args, " "), "--tag derived:latest") &&
					strings.Contains(strings.Join(execution.Cmd.Args, " "), "--progress plain")
			},
		},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 0
		},
	})

	imageReference, applyErr := orchestrator.ApplyImageLayers(ctx, containers.ApplyImageLayersOptions{
		BaseImage: containers.InspectedImage{
			Id:   "sha256:base",
			Tags: []string{"base:latest"},
		},
		Layers: []containers.ImageLayer{{
			Digest:      "layer",
			RawContents: base64.StdEncoding.EncodeToString([]byte("opaque tar bytes")),
		}},
		Tag: "derived:latest",
	})

	require.NoError(t, applyErr)
	require.Equal(t, "derived:latest", imageReference)
	require.Len(t, executor.FindAll([]string{"wslc", "image", "build", "--file"}, "", nil), 1)
}
