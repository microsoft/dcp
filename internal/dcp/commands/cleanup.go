/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	apiv1 "github.com/microsoft/dcp/api/v1"
	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	workloadCleanupLeaseRevalidationInterval = 30 * time.Second
	workloadCleanupStopContainerTimeout      = 10
)

type containerOrchestratorProvider func(runtimeName string) (containers.ContainerOrchestrator, error)

type resolvedContainerOrchestrator struct {
	orchestrator containers.ContainerOrchestrator
	err          error
}

type persistentProcessCleanupRunner interface {
	CheckProcessRunning(handle process.ProcessHandle) error
	StopPersistentProcess(ctx context.Context, exe *apiv1.Executable, record *statestore.PersistentProcessRecord, log logr.Logger) error
}

type cleanupLeaseResource string

func (r cleanupLeaseResource) GetLeaseKey() string {
	return string(r)
}

type cleanupReport struct {
	WorkloadID string                `json:"workloadId"`
	Stopped    cleanupStoppedCounts  `json:"stopped"`
	Failures   []cleanupFailureEntry `json:"failures,omitempty"`
}

type cleanupStoppedCounts struct {
	Containers  int `json:"containers"`
	Executables int `json:"executables"`
	Networks    int `json:"networks"`
}

type cleanupFailureEntry struct {
	Kind        string `json:"kind"`
	ResourceKey string `json:"resourceKey"`
	ResourceID  string `json:"resourceId,omitempty"`
	Error       string `json:"error"`
}

func NewCleanupCommand(log *logger.Logger) *cobra.Command {
	cleanupCmd := &cobra.Command{
		Use:   "cleanup <id>",
		Short: "Stops persistent resources associated with a workload ID.",
		Long:  `Stops persistent containers, executables, and networks associated with a workload ID.`,
		RunE:  cleanup(log.Logger),
		Args:  cobra.ExactArgs(1),
	}

	return cleanupCmd
}

func cleanup(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("cleanup")
		workloadID := strings.TrimSpace(args[0])
		if workloadID == "" {
			return fmt.Errorf("workload ID cannot be empty")
		}

		stateStore, stateStoreErr := statestore.Open(cmd.Context(), statestore.Options{})
		if stateStoreErr != nil {
			return fmt.Errorf("failed to initialize state store: %w", stateStoreErr)
		}
		defer func() {
			if stateStoreCloseErr := stateStore.Close(); stateStoreCloseErr != nil {
				log.Error(stateStoreCloseErr, "Failed to close state store")
			}
		}()

		leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
		if leaseOwnerErr != nil {
			return fmt.Errorf("failed to initialize state store lease owner identity: %w", leaseOwnerErr)
		}

		processExecutor := process.NewOSExecutor(log)
		defer processExecutor.Dispose()

		getContainerOrchestrator := func(runtimeName string) (containers.ContainerOrchestrator, error) {
			return runtimes.FindContainerRuntime(cmd.Context(), runtimeName, log.WithName("ContainerOrchestrator"), processExecutor)
		}

		report, cleanupErr := cleanupWorkloadResources(cmd.Context(), workloadID, stateStore, leaseOwner, getContainerOrchestrator, processExecutor, log)
		encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		if encodeErr != nil {
			return fmt.Errorf("could not write cleanup report: %w", encodeErr)
		}
		if cleanupErr != nil {
			return cmds.NewExitCodeError(cleanupErr, 1)
		}
		return nil
	}
}

func cleanupWorkloadResources(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	getContainerOrchestrator containerOrchestratorProvider,
	processExecutor process.Executor,
	log logr.Logger,
) (cleanupReport, error) {
	report := cleanupReport{WorkloadID: workloadID}
	getContainerOrchestrator = cachedContainerOrchestratorProvider(getContainerOrchestrator)

	containerRecords, containerListErr := stateStore.ListPersistentContainersByWorkloadID(ctx, workloadID)
	if containerListErr != nil {
		return report, containerListErr
	}
	processRecords, processListErr := stateStore.ListPersistentProcessesByWorkloadID(ctx, workloadID)
	if processListErr != nil {
		return report, processListErr
	}
	networkRecords, networkListErr := stateStore.ListPersistentNetworksByWorkloadID(ctx, workloadID)
	if networkListErr != nil {
		return report, networkListErr
	}

	for _, record := range containerRecords {
		resourceID, cleaned, cleanupErr := cleanupPersistentContainerRecord(ctx, workloadID, stateStore, leaseOwner, getContainerOrchestrator, record, log)
		if cleanupErr != nil {
			if resourceID == "" {
				resourceID = record.ContainerID
			}
			report.Failures = append(report.Failures, cleanupFailureEntry{
				Kind:        "container",
				ResourceKey: record.ResourceKey,
				ResourceID:  resourceID,
				Error:       cleanupErr.Error(),
			})
			continue
		}
		if cleaned {
			report.Stopped.Containers++
		}
	}

	processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
	for _, record := range processRecords {
		resourceID, cleaned, cleanupErr := cleanupPersistentProcessRecord(ctx, workloadID, stateStore, leaseOwner, processRunner, record, log)
		if cleanupErr != nil {
			if resourceID == "" {
				resourceID = fmt.Sprintf("%d", record.PID)
			}
			report.Failures = append(report.Failures, cleanupFailureEntry{
				Kind:        "executable",
				ResourceKey: record.ResourceKey,
				ResourceID:  resourceID,
				Error:       cleanupErr.Error(),
			})
			continue
		}
		if cleaned {
			report.Stopped.Executables++
		}
	}

	for _, record := range networkRecords {
		resourceID, cleaned, cleanupErr := cleanupPersistentNetworkRecord(ctx, workloadID, stateStore, leaseOwner, getContainerOrchestrator, record, log)
		if cleanupErr != nil {
			if resourceID == "" {
				resourceID = record.NetworkID
			}
			report.Failures = append(report.Failures, cleanupFailureEntry{
				Kind:        "network",
				ResourceKey: record.ResourceKey,
				ResourceID:  resourceID,
				Error:       cleanupErr.Error(),
			})
			continue
		}
		if cleaned {
			report.Stopped.Networks++
		}
	}

	if len(report.Failures) > 0 {
		return report, fmt.Errorf("failed to clean up %d persistent resource(s)", len(report.Failures))
	}
	return report, nil
}

func cachedContainerOrchestratorProvider(getContainerOrchestrator containerOrchestratorProvider) containerOrchestratorProvider {
	resolvedByRuntime := map[string]resolvedContainerOrchestrator{}
	return func(runtimeName string) (containers.ContainerOrchestrator, error) {
		runtimeName = strings.TrimSpace(runtimeName)
		resolved, ok := resolvedByRuntime[runtimeName]
		if ok {
			return resolved.orchestrator, resolved.err
		}

		orchestrator, resolveErr := getContainerOrchestrator(runtimeName)
		resolvedByRuntime[runtimeName] = resolvedContainerOrchestrator{
			orchestrator: orchestrator,
			err:          resolveErr,
		}
		return orchestrator, resolveErr
	}
}

func cleanupPersistentContainerRecord(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	getContainerOrchestrator containerOrchestratorProvider,
	record statestore.PersistentContainerRecord,
	log logr.Logger,
) (string, bool, error) {
	cleaned := false
	resourceID, _, cleanupErr := withCurrentPersistentContainerRecord(ctx, workloadID, stateStore, leaseOwner, record, func(ctx context.Context, currentRecord *statestore.PersistentContainerRecord) error {
		orchestrator, resolveErr := getContainerOrchestrator(currentRecord.RuntimeName)
		if resolveErr != nil {
			return fmt.Errorf("could not resolve container runtime %q: %w", currentRecord.RuntimeName, resolveErr)
		}

		removeErr := removePersistentContainer(ctx, orchestrator, currentRecord.ContainerID)
		if removeErr != nil {
			return removeErr
		}
		if deleteErr := stateStore.DeletePersistentContainer(ctx, currentRecord.ResourceKey); deleteErr != nil {
			log.Error(deleteErr, "Could not delete persistent Container record", "ResourceKey", currentRecord.ResourceKey)
			return deleteErr
		}
		cleaned = true
		return nil
	})
	return resourceID, cleaned, cleanupErr
}

func withCurrentPersistentContainerRecord(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	record statestore.PersistentContainerRecord,
	f func(context.Context, *statestore.PersistentContainerRecord) error,
) (string, bool, error) {
	var resourceID string
	found := false
	cleanupErr := stateStore.WithResourceLease(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
		currentRecord, getErr := stateStore.GetPersistentContainer(ctx, record.ResourceKey)
		if errors.Is(getErr, statestore.ErrPersistentContainerNotFound) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("could not reload persistent Container record '%s': %w", record.ResourceKey, getErr)
		}
		if currentRecord.WorkloadID != workloadID {
			return nil
		}
		resourceID = currentRecord.ContainerID
		found = true
		if f == nil {
			return nil
		}
		return f(ctx, currentRecord)
	})
	return resourceID, found, cleanupErr
}

func cleanupPersistentProcessRecord(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	processRunner persistentProcessCleanupRunner,
	record statestore.PersistentProcessRecord,
	log logr.Logger,
) (string, bool, error) {
	var resourceID string
	cleaned := false
	cleanupErr := stateStore.WithResourceLease(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
		currentRecord, getErr := stateStore.GetPersistentProcess(ctx, record.ResourceKey)
		if errors.Is(getErr, statestore.ErrPersistentProcessNotFound) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("could not reload persistent Executable process record '%s': %w", record.ResourceKey, getErr)
		}
		if currentRecord.WorkloadID != workloadID {
			return nil
		}
		resourceID = fmt.Sprintf("%d", currentRecord.PID)
		cleaned = true

		deleteRecord := func(deleteLogMessage string) error {
			if deleteErr := stateStore.DeletePersistentProcess(ctx, currentRecord.ResourceKey); deleteErr != nil {
				log.Error(deleteErr, deleteLogMessage, "ResourceKey", currentRecord.ResourceKey)
				return deleteErr
			}
			return nil
		}

		if findErr := processRunner.CheckProcessRunning(currentRecord.ProcessHandle()); findErr != nil {
			if !process.IsProcessGoneErr(findErr) {
				return fmt.Errorf("could not verify persistent Executable process '%s' is running: %w", currentRecord.ResourceKey, findErr)
			}
			return deleteRecord("Could not delete stale persistent Executable process record")
		}

		executable := &apiv1.Executable{}
		executable.Spec.ExecutablePath = currentRecord.ResourceKey
		stopErr := processRunner.StopPersistentProcess(ctx, executable, currentRecord, log)
		if stopErr != nil {
			if process.IsProcessGoneErr(stopErr) {
				return deleteRecord("Could not delete stale persistent Executable process record")
			}
			return stopErr
		}
		return deleteRecord("Could not delete persistent Executable process record")
	})
	return resourceID, cleaned, cleanupErr
}

func cleanupPersistentNetworkRecord(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	getContainerOrchestrator containerOrchestratorProvider,
	record statestore.PersistentNetworkRecord,
	log logr.Logger,
) (string, bool, error) {
	cleaned := false
	resourceID, _, cleanupErr := withCurrentPersistentNetworkRecord(ctx, workloadID, stateStore, leaseOwner, record, func(ctx context.Context, currentRecord *statestore.PersistentNetworkRecord) error {
		orchestrator, resolveErr := getContainerOrchestrator(currentRecord.RuntimeName)
		if resolveErr != nil {
			return fmt.Errorf("could not resolve container runtime %q: %w", currentRecord.RuntimeName, resolveErr)
		}

		removeErr := removePersistentNetwork(ctx, orchestrator, currentRecord.NetworkID)
		if removeErr != nil {
			return removeErr
		}
		if deleteErr := stateStore.DeletePersistentNetwork(ctx, currentRecord.ResourceKey); deleteErr != nil {
			log.Error(deleteErr, "Could not delete persistent ContainerNetwork record", "ResourceKey", currentRecord.ResourceKey)
			return deleteErr
		}
		cleaned = true
		return nil
	})
	return resourceID, cleaned, cleanupErr
}

func withCurrentPersistentNetworkRecord(
	ctx context.Context,
	workloadID string,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	record statestore.PersistentNetworkRecord,
	f func(context.Context, *statestore.PersistentNetworkRecord) error,
) (string, bool, error) {
	var resourceID string
	found := false
	cleanupErr := stateStore.WithResourceLease(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
		currentRecord, getErr := stateStore.GetPersistentNetwork(ctx, record.ResourceKey)
		if errors.Is(getErr, statestore.ErrPersistentNetworkNotFound) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("could not reload persistent ContainerNetwork record '%s': %w", record.ResourceKey, getErr)
		}
		if currentRecord.WorkloadID != workloadID {
			return nil
		}
		resourceID = currentRecord.NetworkID
		found = true
		if f == nil {
			return nil
		}
		return f(ctx, currentRecord)
	})
	return resourceID, found, cleanupErr
}

func removePersistentContainer(ctx context.Context, orchestrator containers.ContainerOrchestrator, containerID string) error {
	if strings.TrimSpace(containerID) == "" {
		return fmt.Errorf("container ID cannot be empty")
	}

	_, stopErr := orchestrator.StopContainers(ctx, containers.StopContainersOptions{
		Containers:    []string{containerID},
		SecondsToKill: workloadCleanupStopContainerTimeout,
	})
	if stopErr != nil && !errors.Is(stopErr, containers.ErrNotFound) {
		// Continue to force remove; a successful remove still reaches the desired cleanup state.
	}

	_, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		return removeErr
	}

	_, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return inspectErr
	}
	return fmt.Errorf("container %s still exists after cleanup", containerID)
}

func removePersistentNetwork(ctx context.Context, orchestrator containers.ContainerOrchestrator, networkID string) error {
	if strings.TrimSpace(networkID) == "" {
		return fmt.Errorf("network ID cannot be empty")
	}

	_, initialInspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	if errors.Is(initialInspectErr, containers.ErrNotFound) {
		return nil
	}
	if initialInspectErr != nil {
		return initialInspectErr
	}

	_, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{networkID},
		Force:    true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		return removeErr
	}

	_, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return inspectErr
	}
	return fmt.Errorf("network %s still exists after cleanup", networkID)
}
