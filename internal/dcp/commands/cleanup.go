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
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	workloadCleanupLeaseRevalidationInterval = 30 * time.Second
	workloadCleanupLeaseRetryInterval        = 500 * time.Millisecond
	workloadCleanupContainerTimeout          = 30 * time.Second
	workloadCleanupNetworkTimeout            = 30 * time.Second
	workloadCleanupStopContainerTimeout      = 10
	workloadCleanupResourceConcurrencyLimit  = uint16(8)
	cleanupVolumesFlagName                   = "volumes"
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

var (
	cleanupResourceContainerGVR  = (&apiv1.Container{}).GetGroupVersionResource()
	cleanupResourceExecutableGVR = (&apiv1.Executable{}).GetGroupVersionResource()
	cleanupResourceNetworkGVR    = (&apiv1.ContainerNetwork{}).GetGroupVersionResource()
	cleanupResourceVolumeGVR     = (&apiv1.ContainerVolume{}).GetGroupVersionResource()
)

type cleanupResourceState uint

const (
	cleanupResourceStateInitial cleanupResourceState = iota
	cleanupResourceStateProcessing
	cleanupResourceStateDone
)

type cleanupResourceGroup struct {
	gvr          schema.GroupVersionResource
	cleanUpAfter []schema.GroupVersionResource
	workItems    []cleanupWorkItem

	state      cleanupResourceState
	waitingFor []schema.GroupVersionResource
}

type cleanupWorkItem struct {
	gvr                schema.GroupVersionResource
	resourceKey        string
	fallbackResourceID string
	clean              func() (string, bool, error)
}

type cleanupWorkResult struct {
	gvr                schema.GroupVersionResource
	resourceKey        string
	fallbackResourceID string
	resourceID         string
	cleaned            bool
	err                error
}

type cleanupResourceGroupResult struct {
	groupIndex int
	results    []cleanupWorkResult
}

type cleanupLeaseResource string

func (r cleanupLeaseResource) GetLeaseKey() string {
	return string(r)
}

var cleanupStoppedCounters = map[schema.GroupVersionResource]func(*cleanupStoppedCounts){
	cleanupResourceContainerGVR: func(counts *cleanupStoppedCounts) {
		counts.Containers++
	},
	cleanupResourceExecutableGVR: func(counts *cleanupStoppedCounts) {
		counts.Executables++
	},
	cleanupResourceNetworkGVR: func(counts *cleanupStoppedCounts) {
		counts.Networks++
	},
	cleanupResourceVolumeGVR: func(counts *cleanupStoppedCounts) {
		counts.Volumes++
	},
}

type cleanupReport struct {
	WorkloadID commonapi.WorkloadID  `json:"workloadId"`
	Stopped    cleanupStoppedCounts  `json:"stopped"`
	Failures   []cleanupFailureEntry `json:"failures,omitempty"`
}

type cleanupStoppedCounts struct {
	Containers  int `json:"containers"`
	Executables int `json:"executables"`
	Networks    int `json:"networks"`
	Volumes     int `json:"volumes"`
}

type cleanupFailureEntry struct {
	Kind        string `json:"kind"`
	ResourceKey string `json:"resourceKey"`
	ResourceID  string `json:"resourceId,omitempty"`
	Error       string `json:"error"`
}

func NewCleanupCommand(log *logger.Logger) *cobra.Command {
	cleanupCmd := &cobra.Command{
		Use:   "cleanup <workload id>",
		Short: "Stops resources associated with a workload ID.",
		Long: fmt.Sprintf(`Stops persistent containers, executables, and networks associated with a workload ID.
It also removes remaining workload-labeled containers on the selected and recorded container runtimes, and workload-labeled networks on the selected runtime.

Persistent volumes are preserved by default. Use --%s to remove associated persistent volumes created by DCP after containers are removed.

Using workload IDs is optional. Workload IDs are trimmed and must be no longer than %d bytes.
See "run-controllers" command for information on how to associate resources with workload IDs.`, cleanupVolumesFlagName, commonapi.MaxWorkloadIDLength),
		RunE: cleanup(log.Logger),
		Args: cobra.ExactArgs(1),
	}
	cleanupCmd.Flags().Bool(cleanupVolumesFlagName, false, "Remove DCP-created persistent volumes associated with the workload ID.")
	container_flags.EnsureRuntimeFlag(cleanupCmd.Flags())

	return cleanupCmd
}

func cleanup(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("cleanup")
		workloadID := commonapi.NormalizeWorkloadID(args[0])
		if workloadID == "" {
			return fmt.Errorf("workload ID cannot be empty")
		}
		if workloadIDErr := workloadID.Validate(); workloadIDErr != nil {
			return workloadIDErr
		}
		options, optionsErr := cleanupWorkloadOptionsFromCommand(cmd)
		if optionsErr != nil {
			return optionsErr
		}

		stateStore, stateStoreErr := statestore.Open(cmd.Context(), statestore.Options{Log: log})
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
		runtimeSpecified := container_flags.GetRuntimeFlagValue() != container_flags.UnknownRuntime
		options.discoverRuntime = func() (containers.ContainerOrchestrator, error) {
			orchestrator, findErr := runtimes.FindAvailableContainerRuntime(cmd.Context(), log.WithName("ContainerOrchestrator"), processExecutor)
			if findErr != nil {
				return nil, findErr
			}
			status := orchestrator.CheckStatus(cmd.Context(), containers.IgnoreCachedRuntimeStatus)
			if !status.Installed && !runtimeSpecified {
				log.Info("No container runtime is installed; skipping workload network discovery")
				return nil, nil
			}
			if !status.Running {
				return nil, fmt.Errorf("container runtime %q is not running: %s", orchestrator.Name(), status.Error)
			}
			return orchestrator, nil
		}

		report, cleanupErr := cleanupWorkloadResources(
			cmd.Context(),
			workloadID,
			stateStore,
			leaseOwner,
			getContainerOrchestrator,
			processExecutor,
			options,
			log,
		)
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

type cleanupWorkloadOptions struct {
	Volumes         bool
	discoverRuntime func() (containers.ContainerOrchestrator, error)
}

func cleanupWorkloadOptionsFromCommand(cmd *cobra.Command) (cleanupWorkloadOptions, error) {
	cleanupVolumes, cleanupVolumesErr := cmd.Flags().GetBool(cleanupVolumesFlagName)
	if cleanupVolumesErr != nil {
		return cleanupWorkloadOptions{}, cleanupVolumesErr
	}
	return cleanupWorkloadOptions{Volumes: cleanupVolumes}, nil
}

func cleanupWorkloadResources(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	getContainerOrchestrator containerOrchestratorProvider,
	processExecutor process.Executor,
	options cleanupWorkloadOptions,
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
	volumeRecords := []statestore.PersistentVolumeRecord{}
	if options.Volumes {
		var volumeListErr error
		volumeRecords, volumeListErr = stateStore.ListPersistentVolumesByWorkloadID(ctx, workloadID)
		if volumeListErr != nil {
			return report, volumeListErr
		}
	}

	processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
	containerWorkItems := make([]cleanupWorkItem, 0, len(containerRecords))
	for _, record := range containerRecords {
		record := record
		containerWorkItems = append(containerWorkItems, cleanupWorkItem{
			gvr:                cleanupResourceContainerGVR,
			resourceKey:        record.ResourceKey,
			fallbackResourceID: record.ContainerID,
			clean: func() (string, bool, error) {
				return cleanupPersistentContainerRecord(ctx, workloadID, stateStore, leaseOwner, getContainerOrchestrator, record, log)
			},
		})
	}
	processWorkItems := make([]cleanupWorkItem, 0, len(processRecords))
	for _, record := range processRecords {
		record := record
		processWorkItems = append(processWorkItems, cleanupWorkItem{
			gvr:                cleanupResourceExecutableGVR,
			resourceKey:        record.ResourceKey,
			fallbackResourceID: fmt.Sprintf("%d", record.PID),
			clean: func() (string, bool, error) {
				return cleanupPersistentProcessRecord(ctx, workloadID, stateStore, leaseOwner, processRunner, record, log)
			},
		})
	}
	networkWorkItems := make([]cleanupWorkItem, 0, len(networkRecords))
	for _, record := range networkRecords {
		record := record
		networkWorkItems = append(networkWorkItems, cleanupWorkItem{
			gvr:                cleanupResourceNetworkGVR,
			resourceKey:        record.ResourceKey,
			fallbackResourceID: record.NetworkID,
			clean: func() (string, bool, error) {
				return cleanupPersistentNetworkRecord(ctx, workloadID, stateStore, leaseOwner, getContainerOrchestrator, record, log)
			},
		})
	}
	volumeWorkItems := make([]cleanupWorkItem, 0, len(volumeRecords))
	for _, record := range volumeRecords {
		record := record
		volumeWorkItems = append(volumeWorkItems, cleanupWorkItem{
			gvr:                cleanupResourceVolumeGVR,
			resourceKey:        record.ResourceKey,
			fallbackResourceID: record.VolumeName,
			clean: func() (string, bool, error) {
				return cleanupPersistentVolumeRecord(ctx, workloadID, stateStore, leaseOwner, getContainerOrchestrator, record, log)
			},
		})
	}

	initialCleanupErr := runCleanupResourceGroups(&report, []cleanupResourceGroup{
		{
			gvr:       cleanupResourceContainerGVR,
			workItems: containerWorkItems,
		},
		{
			gvr:       cleanupResourceExecutableGVR,
			workItems: processWorkItems,
		},
	})

	var discoveredNetworks []containers.ListedNetwork
	var discoveredOrchestrator containers.ContainerOrchestrator
	var discoveryErr error
	if options.discoverRuntime != nil {
		discoveredOrchestrator, discoveryErr = options.discoverRuntime()
		if discoveryErr == nil && discoveredOrchestrator != nil {
			discoveredNetworks, discoveryErr = discoveredOrchestrator.ListNetworks(ctx, containers.ListNetworksOptions{
				Filters: containers.ListNetworksFilters{
					LabelFilters: []containers.LabelFilter{{Key: controllers.WorkloadIDLabel, Value: string(workloadID)}},
				},
			})
		}
		if discoveryErr != nil {
			report.Failures = append(report.Failures, cleanupFailureEntry{
				Kind:        cleanupResourceName(cleanupResourceNetworkGVR),
				ResourceKey: cleanupResourceNetworkGVR.Resource,
				Error:       fmt.Sprintf("could not discover workload networks: %v", discoveryErr),
			})
		}
	}
	recordedNetworks := make(map[string]struct{}, len(networkRecords)*2)
	for _, record := range networkRecords {
		runtimeName := strings.ToLower(strings.TrimSpace(record.RuntimeName))
		recordedNetworks[runtimeName+"\x00"+record.NetworkID] = struct{}{}
		recordedNetworks[runtimeName+"\x00"+record.NetworkName] = struct{}{}
	}
	unrecordedNetworks := make([]containers.ListedNetwork, 0, len(discoveredNetworks))
	if discoveredOrchestrator != nil {
		for _, network := range discoveredNetworks {
			if _, recorded := recordedNetworks[discoveredOrchestrator.Name()+"\x00"+network.ID]; recorded {
				continue
			}
			if _, recorded := recordedNetworks[discoveredOrchestrator.Name()+"\x00"+network.Name]; recorded {
				continue
			}
			unrecordedNetworks = append(unrecordedNetworks, network)
		}
	}
	remainingContainerWorkItems := remainingWorkloadContainerWorkItems(
		ctx,
		workloadID,
		containerRecords,
		networkRecords,
		volumeRecords,
		discoveredOrchestrator,
		getContainerOrchestrator,
	)
	for _, network := range unrecordedNetworks {
		network := network
		networkWorkItems = append(networkWorkItems, cleanupWorkItem{
			gvr:                cleanupResourceNetworkGVR,
			resourceKey:        cleanupResourceNetworkGVR.Resource + "/" + network.Name,
			fallbackResourceID: network.ID,
			clean: func() (string, bool, error) {
				verificationErr := verifyWorkloadContainersRemoved(ctx, workloadID, discoveredOrchestrator, network.ID)
				removeErr := removePersistentNetwork(ctx, discoveredOrchestrator, network.ID)
				networkCleanupErr := errors.Join(verificationErr, removeErr)
				return network.ID, networkCleanupErr == nil, networkCleanupErr
			},
		})
	}
	dependentCleanupErr := runCleanupResourceGroups(&report, []cleanupResourceGroup{
		{
			gvr:       cleanupResourceContainerGVR,
			workItems: remainingContainerWorkItems,
		},
		{
			gvr:          cleanupResourceNetworkGVR,
			cleanUpAfter: []schema.GroupVersionResource{cleanupResourceContainerGVR},
			workItems:    networkWorkItems,
		},
		{
			gvr:          cleanupResourceVolumeGVR,
			cleanUpAfter: []schema.GroupVersionResource{cleanupResourceContainerGVR},
			workItems:    volumeWorkItems,
		},
	})
	return report, errors.Join(initialCleanupErr, discoveryErr, dependentCleanupErr)
}

func remainingWorkloadContainerWorkItems(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	containerRecords []statestore.PersistentContainerRecord,
	networkRecords []statestore.PersistentNetworkRecord,
	volumeRecords []statestore.PersistentVolumeRecord,
	discoveredOrchestrator containers.ContainerOrchestrator,
	getContainerOrchestrator containerOrchestratorProvider,
) []cleanupWorkItem {
	containerWorkItems := []cleanupWorkItem{}
	recordedContainers := make(map[string]struct{}, len(containerRecords))
	for _, record := range containerRecords {
		runtimeName := strings.ToLower(strings.TrimSpace(record.RuntimeName))
		recordedContainers[runtimeName+"\x00"+record.ContainerID] = struct{}{}
	}

	addRuntimeContainers := func(runtimeName string, orchestrator containers.ContainerOrchestrator) {
		listedContainers, listErr := listWorkloadContainers(ctx, workloadID, orchestrator, "")
		if listErr != nil {
			containerWorkItems = append(containerWorkItems, cleanupWorkItem{
				gvr:         cleanupResourceContainerGVR,
				resourceKey: cleanupResourceContainerGVR.Resource + "/" + runtimeName,
				clean: func() (string, bool, error) {
					return "", false, fmt.Errorf("could not list workload containers on runtime %q: %w", runtimeName, listErr)
				},
			})
			return
		}

		for _, listedContainer := range listedContainers {
			containerKey := runtimeName + "\x00" + listedContainer.Id
			if _, recorded := recordedContainers[containerKey]; recorded {
				continue
			}

			containerID := listedContainer.Id
			resourceName := listedContainer.Name
			if resourceName == "" {
				resourceName = containerID
			}
			containerWorkItems = append(containerWorkItems, cleanupWorkItem{
				gvr:                cleanupResourceContainerGVR,
				resourceKey:        cleanupResourceContainerGVR.Resource + "/" + resourceName,
				fallbackResourceID: containerID,
				clean: func() (string, bool, error) {
					removeErr := removeContainerWithRetry(ctx, orchestrator, containerID)
					return containerID, removeErr == nil, removeErr
				},
			})
		}
	}

	seenRuntimes := map[string]struct{}{}
	if discoveredOrchestrator != nil {
		runtimeName := strings.ToLower(strings.TrimSpace(discoveredOrchestrator.Name()))
		seenRuntimes[runtimeName] = struct{}{}
		addRuntimeContainers(runtimeName, discoveredOrchestrator)
	}
	addRecordedRuntime := func(recordRuntime string) {
		runtimeName := strings.ToLower(strings.TrimSpace(recordRuntime))
		if _, seen := seenRuntimes[runtimeName]; seen {
			return
		}
		seenRuntimes[runtimeName] = struct{}{}
		orchestrator, resolveErr := getContainerOrchestrator(recordRuntime)
		if resolveErr != nil {
			containerWorkItems = append(containerWorkItems, cleanupWorkItem{
				gvr:         cleanupResourceContainerGVR,
				resourceKey: cleanupResourceContainerGVR.Resource + "/" + runtimeName,
				clean: func() (string, bool, error) {
					return "", false, fmt.Errorf("could not resolve container runtime %q for workload container discovery: %w", recordRuntime, resolveErr)
				},
			})
			return
		}
		addRuntimeContainers(runtimeName, orchestrator)
	}
	for _, record := range containerRecords {
		addRecordedRuntime(record.RuntimeName)
	}
	for _, record := range networkRecords {
		addRecordedRuntime(record.RuntimeName)
	}
	for _, record := range volumeRecords {
		addRecordedRuntime(record.RuntimeName)
	}

	return containerWorkItems
}

func listWorkloadContainers(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	orchestrator containers.ContainerOrchestrator,
	networkID string,
) ([]containers.ListedContainer, error) {
	filters := containers.ListContainersFilters{
		LabelFilters: []containers.LabelFilter{{Key: controllers.WorkloadIDLabel, Value: string(workloadID)}},
	}
	if networkID != "" {
		filters.NetworkFilters = []string{networkID}
	}
	return orchestrator.ListContainers(ctx, containers.ListContainersOptions{
		All:     true,
		Filters: filters,
	})
}

func runCleanupResourceGroups(report *cleanupReport, groups []cleanupResourceGroup) error {
	initialFailureCount := len(report.Failures)
	results, runCleanupErr := cleanupResourceGroups(groups)
	for _, result := range results {
		if result.err != nil {
			resourceID := result.resourceID
			if resourceID == "" {
				resourceID = result.fallbackResourceID
			}
			report.Failures = append(report.Failures, cleanupFailureEntry{
				Kind:        cleanupResourceName(result.gvr),
				ResourceKey: result.resourceKey,
				ResourceID:  resourceID,
				Error:       result.err.Error(),
			})
			continue
		}
		if !result.cleaned {
			continue
		}
		countStopped, ok := cleanupStoppedCounters[result.gvr]
		if !ok {
			return fmt.Errorf("unknown cleanup resource gvr %q", cleanupResourceName(result.gvr))
		}
		countStopped(&report.Stopped)
	}

	failureErr := cleanupFailuresError(report.Failures[initialFailureCount:])
	if runCleanupErr != nil {
		return errors.Join(runCleanupErr, failureErr)
	}
	return failureErr
}

func cleanupFailuresError(failures []cleanupFailureEntry) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("failed to clean up %d resource(s)", len(failures))
}

func cleanupResourceGroups(groups []cleanupResourceGroup) ([]cleanupWorkResult, error) {
	totalWorkItems := 0
	for i := range groups {
		groups[i].state = cleanupResourceStateInitial
		groups[i].waitingFor = append([]schema.GroupVersionResource(nil), groups[i].cleanUpAfter...)
		totalWorkItems += len(groups[i].workItems)
	}

	results := make([]cleanupWorkResult, 0, totalWorkItems)
	groupDone := make(chan cleanupResourceGroupResult)
	inProgress := 0
	startReadyGroups := func() {
		readyGroupIndexes := cleanupReadyGroupIndexes(groups)
		for _, groupIndex := range readyGroupIndexes {
			groups[groupIndex].state = cleanupResourceStateProcessing
			inProgress++
			go func(groupIndex int, workItems []cleanupWorkItem) {
				groupDone <- cleanupResourceGroupResult{
					groupIndex: groupIndex,
					results:    cleanupWorkItems(workItems),
				}
			}(groupIndex, groups[groupIndex].workItems)
		}
	}

	startReadyGroups()
	for inProgress > 0 {
		groupResult := <-groupDone
		inProgress--
		results = append(results, groupResult.results...)
		groups[groupResult.groupIndex].state = cleanupResourceStateDone
		completedGVR := groups[groupResult.groupIndex].gvr
		for groupIndex := range groups {
			if groups[groupIndex].state == cleanupResourceStateDone {
				continue
			}
			groups[groupIndex].waitingFor = slices.Select(groups[groupIndex].waitingFor, func(gvr schema.GroupVersionResource) bool {
				return gvr != completedGVR
			})
		}
		startReadyGroups()
	}

	blockedGroups := slices.Select(groups, func(group cleanupResourceGroup) bool {
		return group.state != cleanupResourceStateDone
	})
	if len(blockedGroups) > 0 {
		blockedGroupDescriptions := slices.Map[string](blockedGroups, func(group cleanupResourceGroup) string {
			dependencies := slices.Map[string](group.waitingFor, func(gvr schema.GroupVersionResource) string {
				return cleanupResourceName(gvr)
			})
			return fmt.Sprintf("%s waiting for %s", cleanupResourceName(group.gvr), strings.Join(dependencies, ", "))
		})
		return results, fmt.Errorf("could not resolve cleanup resource dependencies: %s", strings.Join(blockedGroupDescriptions, "; "))
	}

	return results, nil
}

func cleanupReadyGroupIndexes(groups []cleanupResourceGroup) []int {
	readyGroupIndexes := make([]int, 0, len(groups))
	for groupIndex, group := range groups {
		if group.state == cleanupResourceStateInitial && len(group.waitingFor) == 0 {
			readyGroupIndexes = append(readyGroupIndexes, groupIndex)
		}
	}
	return readyGroupIndexes
}

func cleanupResourceName(gvr schema.GroupVersionResource) string {
	if gvr.Resource != "" {
		return gvr.Resource
	}
	return gvr.String()
}

func cleanupWorkItems(workItems []cleanupWorkItem) []cleanupWorkResult {
	return slices.MapConcurrent[cleanupWorkResult](workItems, func(workItem cleanupWorkItem) cleanupWorkResult {
		resourceID, cleaned, cleanupErr := workItem.clean()
		return cleanupWorkResult{
			gvr:                workItem.gvr,
			resourceKey:        workItem.resourceKey,
			fallbackResourceID: workItem.fallbackResourceID,
			resourceID:         resourceID,
			cleaned:            cleaned,
			err:                cleanupErr,
		}
	}, workloadCleanupResourceConcurrencyLimit)
}

func cachedContainerOrchestratorProvider(getContainerOrchestrator containerOrchestratorProvider) containerOrchestratorProvider {
	resolvedByRuntime := map[string]resolvedContainerOrchestrator{}
	var resolvedByRuntimeLock sync.Mutex
	return func(runtimeName string) (containers.ContainerOrchestrator, error) {
		runtimeName = strings.TrimSpace(runtimeName)
		resolvedByRuntimeLock.Lock()
		defer resolvedByRuntimeLock.Unlock()

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
	workloadID commonapi.WorkloadID,
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
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	record statestore.PersistentContainerRecord,
	f func(context.Context, *statestore.PersistentContainerRecord) error,
) (string, bool, error) {
	var resourceID string
	found := false
	cleanupErr := stateStore.WithResourceLeaseRetry(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, workloadCleanupLeaseRetryInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
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
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	processRunner persistentProcessCleanupRunner,
	record statestore.PersistentProcessRecord,
	log logr.Logger,
) (string, bool, error) {
	var resourceID string
	cleaned := false
	cleanupErr := stateStore.WithResourceLeaseRetry(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, workloadCleanupLeaseRetryInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
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
	workloadID commonapi.WorkloadID,
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

		verificationErr := verifyWorkloadContainersRemoved(ctx, workloadID, orchestrator, currentRecord.NetworkID)
		removeErr := removePersistentNetwork(ctx, orchestrator, currentRecord.NetworkID)
		if verificationErr != nil || removeErr != nil {
			return errors.Join(verificationErr, removeErr)
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

func verifyWorkloadContainersRemoved(ctx context.Context, workloadID commonapi.WorkloadID, orchestrator containers.ContainerOrchestrator, networkID string) error {
	remainingContainers, listErr := listWorkloadContainers(ctx, workloadID, orchestrator, networkID)
	if listErr != nil {
		_, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("could not verify workload containers were removed from network %q: %w", networkID, errors.Join(listErr, inspectErr))
	}
	if len(remainingContainers) > 0 {
		return fmt.Errorf("%d workload container(s) remain attached to network %q", len(remainingContainers), networkID)
	}
	return nil
}

func withCurrentPersistentNetworkRecord(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	record statestore.PersistentNetworkRecord,
	f func(context.Context, *statestore.PersistentNetworkRecord) error,
) (string, bool, error) {
	var resourceID string
	found := false
	cleanupErr := stateStore.WithResourceLeaseRetry(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, workloadCleanupLeaseRetryInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
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

func cleanupPersistentVolumeRecord(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	getContainerOrchestrator containerOrchestratorProvider,
	record statestore.PersistentVolumeRecord,
	log logr.Logger,
) (string, bool, error) {
	cleaned := false
	resourceID, _, cleanupErr := withCurrentPersistentVolumeRecord(ctx, workloadID, stateStore, leaseOwner, record, func(ctx context.Context, currentRecord *statestore.PersistentVolumeRecord) error {
		orchestrator, resolveErr := getContainerOrchestrator(currentRecord.RuntimeName)
		if resolveErr != nil {
			return fmt.Errorf("could not resolve container runtime %q: %w", currentRecord.RuntimeName, resolveErr)
		}

		removed, removeErr := removePersistentVolume(ctx, orchestrator, currentRecord.VolumeName, currentRecord.OwnershipToken)
		if removeErr != nil {
			return removeErr
		}
		if deleteErr := stateStore.DeletePersistentVolume(ctx, currentRecord.ResourceKey); deleteErr != nil {
			log.Error(deleteErr, "Could not delete persistent ContainerVolume record", "ResourceKey", currentRecord.ResourceKey)
			return deleteErr
		}
		cleaned = removed
		return nil
	})
	return resourceID, cleaned, cleanupErr
}

func withCurrentPersistentVolumeRecord(
	ctx context.Context,
	workloadID commonapi.WorkloadID,
	stateStore *statestore.Store,
	leaseOwner process.ProcessHandle,
	record statestore.PersistentVolumeRecord,
	f func(context.Context, *statestore.PersistentVolumeRecord) error,
) (string, bool, error) {
	var resourceID string
	found := false
	cleanupErr := stateStore.WithResourceLeaseRetry(ctx, cleanupLeaseResource(record.ResourceKey), leaseOwner, workloadCleanupLeaseRevalidationInterval, workloadCleanupLeaseRetryInterval, func(ctx context.Context, _ *statestore.ResourceLease) error {
		currentRecord, getErr := stateStore.GetPersistentVolume(ctx, record.ResourceKey)
		if errors.Is(getErr, statestore.ErrPersistentVolumeNotFound) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("could not reload persistent ContainerVolume record '%s': %w", record.ResourceKey, getErr)
		}
		if currentRecord.WorkloadID != workloadID {
			return nil
		}
		resourceID = currentRecord.VolumeName
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

	// Try a graceful stop first; force removal below can still reach the desired cleanup state if this fails.
	_, _ = orchestrator.StopContainers(ctx, containers.StopContainersOptions{
		Containers:    []string{containerID},
		SecondsToKill: workloadCleanupStopContainerTimeout,
	})

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

func removeContainerWithRetry(ctx context.Context, orchestrator containers.ContainerOrchestrator, containerID string) error {
	return removeContainerWithRetryTimeout(ctx, orchestrator, containerID, workloadCleanupContainerTimeout)
}

func removeContainerWithRetryTimeout(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	containerID string,
	timeout time.Duration,
) error {
	cleanupCtx, cancelCleanupCtx := context.WithTimeout(ctx, timeout)
	defer cancelCleanupCtx()

	return resiliency.RetryExponential(cleanupCtx, func() error {
		return removePersistentContainer(cleanupCtx, orchestrator, containerID)
	})
}

func removePersistentVolume(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	volumeName string,
	ownershipToken string,
) (bool, error) {
	if strings.TrimSpace(volumeName) == "" {
		return false, fmt.Errorf("volume name cannot be empty")
	}

	inspectedVolumes, initialInspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	if errors.Is(initialInspectErr, containers.ErrNotFound) {
		return false, nil
	}
	if initialInspectErr != nil {
		return false, initialInspectErr
	}
	if len(inspectedVolumes) == 0 {
		return false, fmt.Errorf("volume %s could not be inspected before cleanup", volumeName)
	}
	if ownershipToken == "" || inspectedVolumes[0].Labels[containers.VolumeOwnershipTokenLabel] != ownershipToken {
		return false, nil
	}

	_, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{volumeName},
	})
	if errors.Is(removeErr, containers.ErrNotFound) {
		return false, nil
	}
	if removeErr != nil {
		return false, removeErr
	}

	_, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	if errors.Is(inspectErr, containers.ErrNotFound) {
		return true, nil
	}
	if inspectErr != nil {
		return false, inspectErr
	}
	return false, fmt.Errorf("volume %s still exists after cleanup", volumeName)
}

func removePersistentNetwork(ctx context.Context, orchestrator containers.ContainerOrchestrator, networkID string) error {
	if strings.TrimSpace(networkID) == "" {
		return fmt.Errorf("network ID cannot be empty")
	}

	cleanupCtx, cancelCleanupCtx := context.WithTimeout(ctx, workloadCleanupNetworkTimeout)
	defer cancelCleanupCtx()

	return resiliency.RetryExponential(cleanupCtx, func() error {
		inspectedNetworks, inspectErr := orchestrator.InspectNetworks(cleanupCtx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil
		}
		if inspectErr != nil {
			return inspectErr
		}
		if len(inspectedNetworks) == 0 {
			return fmt.Errorf("network %s inspection returned no results", networkID)
		}

		listedContainers, listErr := orchestrator.ListContainers(cleanupCtx, containers.ListContainersOptions{
			All: true,
			Filters: containers.ListContainersFilters{
				NetworkFilters: []string{inspectedNetworks[0].Id},
			},
		})
		if listErr != nil {
			_, confirmErr := orchestrator.InspectNetworks(cleanupCtx, containers.InspectNetworksOptions{
				Networks: []string{networkID},
			})
			if errors.Is(confirmErr, containers.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("list containers attached to network %s: %w", networkID, errors.Join(listErr, confirmErr))
		}

		attachedContainerIDs := make(map[string]struct{}, len(inspectedNetworks[0].Containers)+len(listedContainers))
		for _, attachedContainer := range inspectedNetworks[0].Containers {
			attachedContainerIDs[attachedContainer.Id] = struct{}{}
		}
		for _, listedContainer := range listedContainers {
			attachedContainerIDs[listedContainer.Id] = struct{}{}
		}

		var disconnectErr error
		for attachedContainerID := range attachedContainerIDs {
			containerDisconnectErr := orchestrator.DisconnectNetwork(cleanupCtx, containers.DisconnectNetworkOptions{
				Network:   networkID,
				Container: attachedContainerID,
				Force:     true,
			})
			if containerDisconnectErr != nil && !errors.Is(containerDisconnectErr, containers.ErrNotFound) {
				disconnectErr = errors.Join(disconnectErr, containerDisconnectErr)
			}
		}
		if disconnectErr != nil {
			return disconnectErr
		}

		_, removeErr := orchestrator.RemoveNetworks(cleanupCtx, containers.RemoveNetworksOptions{
			Networks: []string{networkID},
			Force:    true,
		})

		_, verifyErr := orchestrator.InspectNetworks(cleanupCtx, containers.InspectNetworksOptions{Networks: []string{networkID}})
		if errors.Is(verifyErr, containers.ErrNotFound) {
			return nil
		}
		if verifyErr != nil {
			return errors.Join(removeErr, verifyErr)
		}

		return errors.Join(removeErr, fmt.Errorf("network %s still exists after cleanup", networkID))
	})
}
