/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	containerProbePath    = "/usr/local/bin/container-probe"
	testOperationTimeout  = 3 * time.Minute
	finalCleanupTimeout   = 2 * time.Minute
	probeImageLabel       = "com.microsoft.developer.dcp.container-orchestrator-test-probe"
	probeImageTagPrefix   = "localhost/dcp-container-test-probe"
	probeImageHashTagSize = 16
)

type baseImageState struct {
	once  sync.Once
	image string
	err   error
}

var (
	baseImageStatesLock sync.Mutex
	baseImageStates     = map[string]*baseImageState{}
)

func TestMain(testMain *testing.M) {
	exitCode := testMain.Run()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), finalCleanupTimeout)
	cleanupErr := containertest.CleanupActiveResources(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "container orchestrator test cleanup failed: %v\n", cleanupErr)
		exitCode = 1
	}

	os.Exit(exitCode)
}

func ensureTestImage(t *testing.T, ctx context.Context, runtime containertest.Runtime) string {
	t.Helper()

	baseImageStatesLock.Lock()
	state := baseImageStates[runtime.Name]
	if state == nil {
		state = &baseImageState{}
		baseImageStates[runtime.Name] = state
	}
	baseImageStatesLock.Unlock()

	state.once.Do(func() {
		probeBinaryPath, probePathErr := internal_testutil.GetTestToolPath("container_probe_c")
		if probePathErr != nil {
			state.err = fmt.Errorf("finding container probe binary: %w", probePathErr)
			return
		}
		probeHash, hashErr := fileSHA256(probeBinaryPath)
		if hashErr != nil {
			state.err = fmt.Errorf("hashing container probe binary: %w", hashErr)
			return
		}

		probeHashString := fmt.Sprintf("%x", probeHash)
		// Content-derived tags let overlapping test runs safely share this small fixture image.
		state.image = fmt.Sprintf("%s:%x", probeImageTagPrefix, probeHash[:probeImageHashTagSize/2])
		inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{state.image},
		})
		if inspectErr == nil && len(inspected) == 1 && inspected[0].Labels[probeImageLabel] == probeHashString {
			return
		}
		if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
			state.err = fmt.Errorf("inspecting container probe image %q: %w", state.image, inspectErr)
			return
		}

		contextDirectory := t.TempDir()
		probeDestination := filepath.Join(contextDirectory, "container-probe")
		if copyErr := copyTestTool(probeBinaryPath, probeDestination); copyErr != nil {
			state.err = fmt.Errorf("copying container probe into build context: %w", copyErr)
			return
		}
		dockerfilePath := filepath.Join(contextDirectory, "Dockerfile")
		dockerfile := fmt.Sprintf(
			"FROM scratch\nCOPY --chmod=0755 container-probe %[1]s\nWORKDIR /tmp\nENTRYPOINT [\"%[1]s\"]\n",
			containerProbePath,
		)
		if dockerfileErr := usvc_io.WriteFile(dockerfilePath, []byte(dockerfile), osutil.PermissionOnlyOwnerReadWrite); dockerfileErr != nil {
			state.err = fmt.Errorf("writing container probe Dockerfile: %w", dockerfileErr)
			return
		}

		buildErr := runtime.Orchestrator.BuildImage(ctx, containers.BuildImageOptions{
			ContainerBuildContext: &containers.ContainerBuildContext{
				Context:    contextDirectory,
				Dockerfile: dockerfilePath,
				Tags:       []string{state.image},
				Labels: []containers.Label{{
					Key:   probeImageLabel,
					Value: probeHashString,
				}},
			},
		})
		if buildErr != nil {
			state.err = fmt.Errorf("building container probe image %q: %w", state.image, buildErr)
			return
		}

		inspected, inspectErr = runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{state.image},
		})
		if inspectErr != nil {
			state.err = fmt.Errorf("inspecting built container probe image %q: %w", state.image, inspectErr)
			return
		}
		if len(inspected) != 1 || inspected[0].Id == "" || inspected[0].Labels[probeImageLabel] != probeHashString {
			state.err = fmt.Errorf("built container probe image %q was not returned consistently by inspection", state.image)
		}
	})

	if state.err != nil {
		t.Fatal(state.err)
	}
	return state.image
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return [sha256.Size]byte{}, openErr
	}
	defer file.Close()

	hash := sha256.New()
	if _, copyErr := io.Copy(hash, file); copyErr != nil {
		return [sha256.Size]byte{}, copyErr
	}

	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}
