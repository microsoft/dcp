/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildImageImplBuildsCommand(t *testing.T) {
	result := &fakeBuildResult{}
	runner := newFakeRunner(result, "", "", nil)
	timeout := 3 * time.Minute

	errBuf, buildErr := BuildImageImpl(
		t.Context(),
		BuildImageOptions{
			IidFile: "image-id",
			Pull:    true,
			ContainerBuildContext: &ContainerBuildContext{
				Context:    "context-dir",
				Dockerfile: "Containerfile",
				Tags:       []string{"example:first", "example:second"},
				Args: []EnvVar{
					{Name: "SET", Value: "value"},
					{Name: "UNSET"},
				},
				Secrets: []ContainerBuildSecret{
					{Type: FileSecret, ID: "file-secret", Source: "secret.txt"},
					{Type: EnvSecret, ID: "env-secret", Source: "SOURCE_ENV", Value: "secret-value"},
					{Type: EnvSecret, ID: "default-env-secret", Value: "default-secret-value"},
				},
				Stage:    "final",
				Labels:   []Label{{Key: "key", Value: "value"}},
				Platform: "linux/amd64",
			},
			TimeoutOption: TimeoutOption{Timeout: timeout},
		},
		runner,
		"--runtime-option",
	)

	require.NoError(t, buildErr)
	require.NotNil(t, errBuf)
	assert.Equal(t, "BuildImage", result.operationName)
	assert.Equal(t, timeout, result.timeout)
	assert.Equal(t, []string{
		"echo",
		"build",
		"-f", "Containerfile",
		"--pull",
		"--iidfile", "image-id",
		"-t", "example:first",
		"-t", "example:second",
		"--build-arg", "SET=value",
		"--build-arg", "UNSET",
		"--secret", "id=file-secret,src=secret.txt",
		"--secret", "id=env-secret,env=SOURCE_ENV",
		"--secret", "id=default-env-secret,env=default-env-secret",
		"--target", "final",
		"--label", "key=value",
		"--platform", "linux/amd64",
		"--runtime-option",
		"context-dir",
	}, result.args)
	assert.Contains(t, result.env, "SOURCE_ENV=secret-value")
	assert.Contains(t, result.env, "default-env-secret=default-secret-value")
	assert.Empty(t, result.stdinTar)
}

func TestBuildImageImplStreamsArchive(t *testing.T) {
	result := &fakeBuildResult{}
	archiveContents := []byte("archive contents")

	_, buildErr := BuildImageImpl(
		t.Context(),
		BuildImageOptions{
			ContainerBuildContext: &ContainerBuildContext{
				ContextArchive: &ContainerBuildContextArchive{
					Digest:      "archive-digest",
					RawContents: base64.StdEncoding.EncodeToString(archiveContents),
				},
			},
		},
		newFakeRunner(result, "", "", nil),
	)

	require.NoError(t, buildErr)
	assert.Equal(t, []string{"echo", "build", "-"}, result.args)
	assert.Equal(t, archiveContents, result.stdinTar)
	assert.Equal(t, defaultBuildImageTimeout, result.timeout)
}

func TestBuildImageImplRejectsPathAndArchive(t *testing.T) {
	result := &fakeBuildResult{}

	_, buildErr := BuildImageImpl(
		t.Context(),
		BuildImageOptions{
			ContainerBuildContext: &ContainerBuildContext{
				Context: "context-dir",
				ContextArchive: &ContainerBuildContextArchive{
					Digest:      "archive-digest",
					RawContents: base64.StdEncoding.EncodeToString([]byte("archive contents")),
				},
			},
		},
		newFakeRunner(result, "", "", nil),
	)

	require.EqualError(t, buildErr, "build context path and build context archive are mutually exclusive")
	assert.Empty(t, result.args)
}

func TestBuildImageImplReturnsStderrOnFailure(t *testing.T) {
	result := &fakeBuildResult{}
	expectedErr := errors.New("build failed")

	errBuf, buildErr := BuildImageImpl(
		t.Context(),
		BuildImageOptions{ContainerBuildContext: &ContainerBuildContext{Context: "context-dir"}},
		newFakeRunner(result, "", "runtime error", expectedErr),
	)

	require.ErrorIs(t, buildErr, expectedErr)
	require.NotNil(t, errBuf)
	assert.Equal(t, "runtime error", errBuf.String())
}
