/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
)

const (
	baseImageReference   = "docker.io/library/busybox:1.36.1"
	pullImageReference   = "docker.io/library/hello-world:latest"
	testOperationTimeout = 3 * time.Minute
	finalCleanupTimeout  = 2 * time.Minute
)

type baseImageState struct {
	once sync.Once
	err  error
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

func ensureBaseImage(t *testing.T, ctx context.Context, runtime containertest.Runtime) string {
	t.Helper()

	baseImageStatesLock.Lock()
	state := baseImageStates[runtime.Name]
	if state == nil {
		state = &baseImageState{}
		baseImageStates[runtime.Name] = state
	}
	baseImageStatesLock.Unlock()

	state.once.Do(func() {
		inspected, inspectErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{baseImageReference},
		})
		if inspectErr == nil && len(inspected) == 1 {
			return
		}
		if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
			state.err = fmt.Errorf("inspecting base image %q: %w", baseImageReference, inspectErr)
			return
		}

		// Fixed image references can be shared by overlapping test runs, so leave this cached.
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image: baseImageReference,
		})
		if pullErr != nil {
			state.err = fmt.Errorf("pulling base image %q: %w", baseImageReference, pullErr)
			return
		}
		if imageID == "" {
			state.err = fmt.Errorf("pulling base image %q returned an empty image ID", baseImageReference)
			return
		}

		inspected, inspectErr = runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{baseImageReference},
		})
		if inspectErr != nil {
			state.err = fmt.Errorf("inspecting pulled base image %q: %w", baseImageReference, inspectErr)
			return
		}
		if len(inspected) != 1 || inspected[0].Id == "" {
			state.err = fmt.Errorf("pulled base image %q was not returned consistently by inspection", baseImageReference)
		}
	})

	if state.err != nil {
		t.Fatal(state.err)
	}
	return baseImageReference
}
