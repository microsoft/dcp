/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package containertest provides test infrastructure for exercising real container runtimes.
package containertest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	container_runtimes "github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const runtimeDetectionTimeout = 45 * time.Second

var supportedRuntimeNames = []string{
	string(container_flags.DockerRuntime),
	string(container_flags.PodmanRuntime),
}

var runtimeTestSlots = map[string]chan struct{}{
	string(container_flags.DockerRuntime): make(chan struct{}, 8),
	string(container_flags.PodmanRuntime): make(chan struct{}, 4),
}

type runtimeDetectionResult struct {
	name   string
	status containers.ContainerRuntimeStatus
	err    error
}

type runtimeDetectionCache struct {
	once     sync.Once
	runtimes []runtimeDetectionResult
}

type runtimeRecoveryState struct {
	once       sync.Once
	reportOnce sync.Once
	err        error
}

var (
	detectedRuntimes runtimeDetectionCache
	runtimeRecovery  = map[string]*runtimeRecoveryState{
		string(container_flags.DockerRuntime): {},
		string(container_flags.PodmanRuntime): {},
	}
)

// Runtime supplies a healthy orchestrator and its stable runtime name to a runtime subtest.
type Runtime struct {
	Name         string
	Orchestrator containers.ContainerOrchestrator
}

// ForEachHealthyRuntime runs the callback as a parallel subtest for every supported runtime
// that is installed and running. testCtx bounds runtime discovery, setup, and test execution.
func ForEachHealthyRuntime(
	t *testing.T,
	testCtx context.Context,
	run func(t *testing.T, runtimeCtx context.Context, runtime Runtime),
) {
	t.Helper()
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)
	dcppaths.EnableTestPathProbing()

	results := detectContainerRuntimes(testCtx)
	if testCtxErr := testCtx.Err(); testCtxErr != nil {
		t.Fatalf("test context ended while detecting container runtimes: %v", testCtxErr)
	}

	healthyCount := 0
	for _, result := range results {
		if result.err == nil && result.status.IsHealthy() {
			healthyCount++
		}
	}
	if healthyCount == 0 {
		t.Skip("no supported, healthy container runtime is available")
	}

	for _, result := range results {
		result := result
		if result.err != nil {
			t.Logf("Skipping container runtime %q because detection failed: %v", result.name, result.err)
			continue
		}
		if !result.status.IsHealthy() {
			t.Logf("Skipping container runtime %q because it is not healthy: %+v", result.name, result.status)
			continue
		}

		t.Run(result.name, func(t *testing.T) {
			t.Parallel()

			runtimeCtx, runtimeCancel := context.WithCancel(testCtx)
			defer runtimeCancel()

			slot := runtimeTestSlots[result.name]
			select {
			case slot <- struct{}{}:
				defer func() { <-slot }()
			case <-runtimeCtx.Done():
				t.Fatalf("timed out waiting to run against container runtime %q: %v", result.name, runtimeCtx.Err())
			}

			log := testutil.NewLogForTesting(t.Name())
			executor := process.NewOSExecutor(log)
			t.Cleanup(executor.Dispose)

			statusCtx, statusCancel := context.WithTimeout(runtimeCtx, runtimeDetectionTimeout)
			defer statusCancel()

			orchestrator, orchestratorErr := container_runtimes.FindContainerRuntime(
				statusCtx,
				result.name,
				log.WithName("ContainerOrchestrator"),
				executor,
			)
			if orchestratorErr != nil {
				t.Fatalf("could not create %s container orchestrator: %v", result.name, orchestratorErr)
			}

			status := orchestrator.CheckStatus(statusCtx, containers.IgnoreCachedRuntimeStatus)
			if !status.IsHealthy() {
				t.Skipf("container runtime %q became unhealthy: %+v", result.name, status)
			}

			recoveryState := runtimeRecovery[result.name]
			recoveryState.once.Do(func() {
				recoveryState.err = RecoverStaleResources(statusCtx, result.name, orchestrator)
			})
			if recoveryState.err != nil {
				recoveryState.reportOnce.Do(func() {
					t.Logf(
						"Could not recover all stale resources for runtime %q; journals were retained for a future retry: %v",
						result.name,
						recoveryState.err,
					)
				})
			}

			if runtimeCtxErr := runtimeCtx.Err(); runtimeCtxErr != nil {
				t.Fatalf("test context ended while preparing container runtime %q: %v", result.name, runtimeCtxErr)
			}

			run(t, runtimeCtx, Runtime{
				Name:         result.name,
				Orchestrator: orchestrator,
			})
		})
	}
}

func detectContainerRuntimes(testCtx context.Context) []runtimeDetectionResult {
	detectedRuntimes.once.Do(func() {
		results := make([]runtimeDetectionResult, len(supportedRuntimeNames))
		var waitGroup sync.WaitGroup
		waitGroup.Add(len(supportedRuntimeNames))

		for resultIndex, runtimeName := range supportedRuntimeNames {
			resultIndex := resultIndex
			runtimeName := runtimeName

			go func() {
				defer waitGroup.Done()

				log := testutil.NewLogForTesting("detect-" + runtimeName)
				executor := process.NewOSExecutor(log)
				defer executor.Dispose()

				detectionCtx, detectionCancel := context.WithTimeout(testCtx, runtimeDetectionTimeout)
				defer detectionCancel()

				orchestrator, orchestratorErr := container_runtimes.FindContainerRuntime(
					detectionCtx,
					runtimeName,
					log.WithName("ContainerOrchestrator"),
					executor,
				)
				if orchestratorErr != nil {
					results[resultIndex] = runtimeDetectionResult{
						name: runtimeName,
						err:  fmt.Errorf("creating orchestrator: %w", orchestratorErr),
					}
					return
				}

				results[resultIndex] = runtimeDetectionResult{
					name:   runtimeName,
					status: orchestrator.CheckStatus(detectionCtx, containers.CachedRuntimeStatusAllowed),
				}
			}()
		}

		waitGroup.Wait()
		detectedRuntimes.runtimes = results
	})

	return append([]runtimeDetectionResult(nil), detectedRuntimes.runtimes...)
}
