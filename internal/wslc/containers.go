/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/termpty"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
)

func applyCreateContainerOptions(args []string, options containers.CreateContainerOptions) ([]string, error) {
	if options.Image == "" {
		return nil, fmt.Errorf("must specify an image")
	}
	if options.RestartPolicy != "" && options.RestartPolicy != containers.RestartPolicyNone {
		return nil, fmt.Errorf("wslc does not support restart policy %q", options.RestartPolicy)
	}
	if options.Healthcheck.Interval < 0 ||
		options.Healthcheck.Timeout < 0 ||
		options.Healthcheck.StartPeriod < 0 ||
		options.Healthcheck.StartInterval < 0 ||
		options.Healthcheck.Retries < 0 {
		return nil, fmt.Errorf("health-check durations and retry count cannot be negative")
	}
	if options.Healthcheck.StartInterval > 0 {
		return nil, fmt.Errorf("wslc does not support health-check start intervals")
	}
	if len(options.Healthcheck.Command) == 0 &&
		(options.Healthcheck.Interval > 0 ||
			options.Healthcheck.Timeout > 0 ||
			options.Healthcheck.Retries > 0 ||
			options.Healthcheck.StartPeriod > 0) {
		return nil, fmt.Errorf("health-check options require a health-check command")
	}

	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	for _, network := range options.Networks {
		if network.Name == "" {
			return nil, fmt.Errorf("container network name cannot be empty")
		}
		networkValue := network.Name
		if len(network.Aliases) > 0 {
			networkValue = "name=" + network.Name
			for _, alias := range network.Aliases {
				if alias == "" {
					return nil, fmt.Errorf("container network alias cannot be empty")
				}
				networkValue += ",alias=" + alias
			}
		}
		args = append(args, "--network", networkValue)
	}

	for _, mount := range options.VolumeMounts {
		if mount.Type != containers.BindMount && mount.Type != containers.NamedVolumeMount {
			return nil, fmt.Errorf("unsupported container mount type %q", mount.Type)
		}
		if mount.Target == "" {
			return nil, fmt.Errorf("container mount target cannot be empty")
		}

		mountValue := fmt.Sprintf("type=%s", mount.Type)
		if mount.Source != "" {
			mountValue += ",src=" + mount.Source
		}
		mountValue += ",target=" + mount.Target
		if mount.ReadOnly {
			mountValue += ",readonly"
		}
		args = append(args, "--mount", mountValue)
	}

	for _, port := range options.Ports {
		if port.ContainerPort <= 0 {
			return nil, fmt.Errorf("container port must be positive")
		}
		if port.HostPort < 0 {
			return nil, fmt.Errorf("host port cannot be negative")
		}

		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = networking.IPv4LocalhostDefaultAddress
		}

		hostPort := ""
		if port.HostPort > 0 {
			hostPort = fmt.Sprintf("%d", port.HostPort)
		}
		portValue := fmt.Sprintf("%s:%s:%d", hostIP, hostPort, port.ContainerPort)
		if port.Protocol != "" {
			portValue += "/" + port.Protocol
		}
		args = append(args, "--publish", portValue)
	}

	for _, envVar := range options.Env {
		if envVar.Name == "" {
			return nil, fmt.Errorf("container environment variable name cannot be empty")
		}
		args = append(args, "--env", envVar.Name+"="+envVar.Value)
	}
	for _, envFile := range options.EnvFiles {
		if envFile == "" {
			return nil, fmt.Errorf("container environment file path cannot be empty")
		}
		args = append(args, "--env-file", envFile)
	}
	var labelArgsErr error
	args, labelArgsErr = appendLabelArgs(args, options.Labels, "container")
	if labelArgsErr != nil {
		return nil, labelArgsErr
	}

	switch options.PullPolicy {
	case "", containers.PullPolicyAlways, containers.PullPolicyMissing, containers.PullPolicyNever:
	default:
		return nil, fmt.Errorf("unsupported image pull policy %q", options.PullPolicy)
	}
	if options.PullPolicy != "" {
		args = append(args, "--pull", string(options.PullPolicy))
	}

	if options.Entrypoint != "" {
		args = append(args, "--entrypoint", options.Entrypoint)
	}

	if len(options.Healthcheck.Command) > 0 {
		args = append(args, "--health-cmd", strings.Join(options.Healthcheck.Command, " "))
		if options.Healthcheck.Interval > 0 {
			args = append(args, "--health-interval", options.Healthcheck.Interval.String())
		} else {
			args = append(args, "--health-interval", "30s")
		}
		if options.Healthcheck.Timeout > 0 {
			args = append(args, "--health-timeout", options.Healthcheck.Timeout.String())
		}
		if options.Healthcheck.Retries > 0 {
			args = append(args, "--health-retries", fmt.Sprintf("%d", options.Healthcheck.Retries))
		} else {
			args = append(args, "--health-retries", "3")
		}
		if options.Healthcheck.StartPeriod > 0 {
			args = append(args, "--health-start-period", options.Healthcheck.StartPeriod.String())
		}
	}

	if options.AttachTerminal {
		args = append(args, "--interactive", "--tty")
	}

	args = append(args, options.RunArgs...)
	return args, nil
}

func (wco *WslcCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	resolvedOptions, resolveNetworksErr := wco.resolveCreateContainerNetworks(ctx, options)
	if resolveNetworksErr != nil {
		return "", resolveNetworksErr
	}

	args, applyErr := applyCreateContainerOptions([]string{"container", "create"}, resolvedOptions)
	if applyErr != nil {
		return "", applyErr
	}
	args = append(args, resolvedOptions.Image)
	args = append(args, resolvedOptions.Command...)

	timeout := resolvedOptions.Timeout
	if timeout == 0 {
		timeout = defaultCreateTimeout
	}

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"CreateContainer",
		cmd,
		resolvedOptions.StdOutStream,
		resolvedOptions.StdErrStream,
		timeout,
	)
	if runErr != nil {
		operationErr := errors.Join(
			runErr,
			normalizeCliErrors(errBuf, containerNotFoundMatch, imageNotFoundMatch, alreadyExistsMatch, allocationFailureMatch),
		)
		containerID, idErr := parseSingleIdentifier(outBuf)
		if idErr == nil {
			return containerID, operationErr
		}
		return "", operationErr
	}

	return parseSingleIdentifier(outBuf)
}

func (wco *WslcCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	resolvedOptions, resolveNetworksErr := wco.resolveCreateContainerNetworks(ctx, options.CreateContainerOptions)
	if resolveNetworksErr != nil {
		return "", resolveNetworksErr
	}

	createOptions := resolvedOptions
	runArgs := createOptions.RunArgs
	createOptions.RunArgs = nil
	args, applyErr := applyCreateContainerOptions([]string{"container", "run"}, createOptions)
	if applyErr != nil {
		return "", applyErr
	}
	args = append(args, "--detach")
	args = append(args, runArgs...)
	args = append(args, resolvedOptions.Image)
	args = append(args, resolvedOptions.Command...)

	timeout := resolvedOptions.Timeout
	if timeout == 0 {
		timeout = defaultRunTimeout
	}

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"RunContainer",
		cmd,
		resolvedOptions.StdOutStream,
		resolvedOptions.StdErrStream,
		timeout,
	)
	if runErr != nil {
		operationErr := errors.Join(
			runErr,
			normalizeCliErrors(errBuf, containerNotFoundMatch, imageNotFoundMatch, alreadyExistsMatch, allocationFailureMatch),
		)
		containerID, idErr := parseSingleIdentifier(outBuf)
		if idErr == nil {
			return containerID, operationErr
		}
		return "", operationErr
	}

	return parseSingleIdentifier(outBuf)
}

func (wco *WslcCliOrchestrator) resolveCreateContainerNetworks(
	ctx context.Context,
	options containers.CreateContainerOptions,
) (containers.CreateContainerOptions, error) {
	if len(options.Networks) == 0 {
		return options, nil
	}

	resolvedNetworks := make([]containers.CreateContainerNetworkOptions, len(options.Networks))
	for index, requestedNetwork := range options.Networks {
		resolvedNetwork, resolveErr := wco.resolveNetwork(ctx, requestedNetwork.Name)
		if resolveErr != nil {
			return containers.CreateContainerOptions{}, fmt.Errorf(
				"resolving initial container network %q: %w",
				requestedNetwork.Name,
				resolveErr,
			)
		}

		resolvedNetworks[index] = containers.CreateContainerNetworkOptions{
			Name:    resolvedNetwork.Name,
			Aliases: append([]string(nil), requestedNetwork.Aliases...),
		}
	}

	options.Networks = resolvedNetworks
	return options, nil
}

func applyListContainersOptions(args []string, options containers.ListContainersOptions) []string {
	if options.All {
		args = append(args, "--all")
	}
	for _, label := range options.Filters.LabelFilters {
		filter := "label=" + label.Key
		if label.Value != "" {
			filter += "=" + label.Value
		}
		args = append(args, "--filter", filter)
	}
	for _, network := range options.Filters.NetworkFilters {
		args = append(args, "--filter", "network="+network)
	}
	return args
}

func (wco *WslcCliOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	resolvedOptions, resolveFiltersErr := wco.resolveListContainerNetworkFilters(ctx, options)
	if resolveFiltersErr != nil {
		return nil, resolveFiltersErr
	}

	args := applyListContainersOptions([]string{"container", "list", "--no-trunc"}, resolvedOptions)
	args = append(args, "--format", "json")

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"ListContainers",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return nil, errors.Join(runErr, normalizeCliErrors(errBuf))
	}

	rawContainers, decodeErr := decodeJSONLines[wslcListedContainer](outBuf)
	listedContainers := make([]containers.ListedContainer, 0, len(rawContainers))
	labelInspectionIDs := make([]string, 0, len(rawContainers))
	for _, rawContainer := range rawContainers {
		if rawContainer.ID == "" {
			decodeErr = errors.Join(
				decodeErr,
				containers.ErrUnmarshalling,
				fmt.Errorf("listed WSLC container did not contain an ID"),
			)
			continue
		}

		containerName := strings.TrimPrefix(strings.TrimSpace(strings.Split(rawContainer.Names, ",")[0]), "/")
		listedContainers = append(listedContainers, containers.ListedContainer{
			Id:       rawContainer.ID,
			Name:     containerName,
			Image:    rawContainer.Image,
			Status:   rawContainer.State,
			Networks: splitCommaSeparated(rawContainer.Networks),
		})
		labelInspectionIDs = append(labelInspectionIDs, rawContainer.ID)
	}

	if len(labelInspectionIDs) > 0 {
		inspectedContainers, inspectErr := wco.inspectContainersRaw(ctx, labelInspectionIDs)
		labelsByID := make(map[string]map[string]string, len(inspectedContainers))
		for _, inspectedContainer := range inspectedContainers {
			labelsByID[inspectedContainer.ID] = inspectedContainer.Config.Labels
		}
		for index := range listedContainers {
			listedContainers[index].Labels = labelsByID[listedContainers[index].Id]
		}
		if inspectErr != nil || len(inspectedContainers) < len(labelInspectionIDs) {
			decodeErr = errors.Join(
				decodeErr,
				fmt.Errorf("resolving authoritative labels for listed WSLC containers: %w",
					errors.Join(inspectErr, incompleteError("containers", len(inspectedContainers), len(labelInspectionIDs)))),
			)
		}
	}

	return listedContainers, decodeErr
}

func (wco *WslcCliOrchestrator) resolveListContainerNetworkFilters(
	ctx context.Context,
	options containers.ListContainersOptions,
) (containers.ListContainersOptions, error) {
	if len(options.Filters.NetworkFilters) == 0 {
		return options, nil
	}

	resolvedFilters := make([]string, 0, len(options.Filters.NetworkFilters))
	for _, networkReference := range options.Filters.NetworkFilters {
		network, resolveErr := wco.resolveNetwork(ctx, networkReference)
		if resolveErr != nil {
			return containers.ListContainersOptions{}, fmt.Errorf(
				"resolving WSLC container-list network filter %q: %w",
				networkReference,
				resolveErr,
			)
		}
		resolvedFilters = append(resolvedFilters, network.Name)
	}

	options.Filters.NetworkFilters = resolvedFilters
	return options, nil
}

func (wco *WslcCliOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	rawContainers, inspectErr := wco.inspectContainersRaw(ctx, options.Containers)

	networkNamesSet := make(map[string]struct{})
	for _, rawContainer := range rawContainers {
		for networkName := range rawContainer.NetworkSettings.Networks {
			if networkName != "" {
				networkNamesSet[networkName] = struct{}{}
			}
		}
	}

	networkIDs := make(map[string]string, len(networkNamesSet))
	var networkResolutionErr error
	if len(networkNamesSet) > 0 {
		networkNames := make([]string, 0, len(networkNamesSet))
		for networkName := range networkNamesSet {
			networkNames = append(networkNames, networkName)
		}
		sort.Strings(networkNames)

		inspectedNetworks, resolveErr := wco.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: networkNames,
		})
		for _, inspectedNetwork := range inspectedNetworks {
			networkIDs[inspectedNetwork.Name] = inspectedNetwork.Id
		}
		if resolveErr != nil {
			networkResolutionErr = fmt.Errorf("resolving WSLC container network IDs: %w", resolveErr)
		}
	}

	inspectedContainers := make([]containers.InspectedContainer, 0, len(rawContainers))
	var conversionErr error
	for _, rawContainer := range rawContainers {
		inspectedContainer, convertErr := convertInspectedContainer(rawContainer, networkIDs)
		if convertErr != nil {
			conversionErr = errors.Join(conversionErr, containers.ErrUnmarshalling, convertErr)
			continue
		}
		inspectedContainers = append(inspectedContainers, inspectedContainer)
	}

	return inspectedContainers, errors.Join(
		inspectErr,
		networkResolutionErr,
		conversionErr,
		incompleteError("containers", len(inspectedContainers), len(options.Containers)),
	)
}

func (wco *WslcCliOrchestrator) inspectContainersRaw(
	ctx context.Context,
	containerReferences []string,
) ([]wslcInspectedContainer, error) {
	args := append([]string{"container", "inspect", "--format", "json"}, containerReferences...)
	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"InspectContainers",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)

	rawContainers, decodeErr := decodeJSONArray[wslcInspectedContainer](outBuf)
	if runErr != nil {
		runErr = errors.Join(
			runErr,
			normalizeCliErrors(errBuf, containerNotFoundMatch.MaxObjects(len(containerReferences))),
		)
	}
	return rawContainers, errors.Join(runErr, decodeErr)
}

func convertInspectedContainer(
	rawContainer wslcInspectedContainer,
	networkIDs map[string]string,
) (containers.InspectedContainer, error) {
	containerID := strings.TrimSpace(rawContainer.ID)
	containerName := strings.TrimPrefix(strings.TrimSpace(rawContainer.Name), "/")
	if containerID == "" {
		return containers.InspectedContainer{}, fmt.Errorf("inspected WSLC container did not contain an ID")
	}
	if containerName == "" {
		return containers.InspectedContainer{}, fmt.Errorf("inspected WSLC container %q did not contain a name", containerID)
	}

	status := rawContainer.State.Status
	if status == "" && rawContainer.State.Running {
		status = containers.ContainerStatusRunning
	}

	inspectedContainer := containers.InspectedContainer{
		Id:          containerID,
		Name:        containerName,
		Image:       rawContainer.Config.Image,
		CreatedAt:   rawContainer.Created.Time,
		StartedAt:   rawContainer.State.StartedAt.Time,
		FinishedAt:  rawContainer.State.FinishedAt.Time,
		Status:      status,
		Error:       rawContainer.State.Error,
		ExitCode:    rawContainer.State.ExitCode,
		Healthcheck: rawContainer.Config.Healthcheck.Test,
		Health:      rawContainer.State.Health,
		Labels:      rawContainer.Config.Labels,
	}

	inspectedContainer.Env = make(map[string]string, len(rawContainer.Config.Env))
	for _, envValue := range rawContainer.Config.Env {
		name, value, found := strings.Cut(envValue, "=")
		if found {
			inspectedContainer.Env[name] = value
		} else {
			inspectedContainer.Env[envValue] = ""
		}
	}

	inspectedContainer.Args = append(inspectedContainer.Args, rawContainer.Config.Entrypoint...)
	inspectedContainer.Args = append(inspectedContainer.Args, rawContainer.Config.Cmd...)

	for _, rawMount := range rawContainer.Mounts {
		source := rawMount.Source
		if rawMount.Type == containers.NamedVolumeMount {
			source = rawMount.Name
		}
		inspectedContainer.Mounts = append(inspectedContainer.Mounts, containers.VolumeMount{
			Type:     rawMount.Type,
			Source:   source,
			Target:   rawMount.Destination,
			ReadOnly: !rawMount.ReadWrite,
		})
	}

	if rawContainer.Ports != nil {
		inspectedContainer.Ports = make(containers.InspectedContainerPortMapping)
		for portAndProtocol, bindings := range rawContainer.Ports {
			if portAndProtocol == "" || len(bindings) == 0 {
				continue
			}
			inspectedContainer.Ports[portAndProtocol] = bindings
		}
	}

	networkNames := make([]string, 0, len(rawContainer.NetworkSettings.Networks))
	for networkName := range rawContainer.NetworkSettings.Networks {
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)
	for _, networkName := range networkNames {
		rawNetwork := rawContainer.NetworkSettings.Networks[networkName]
		inspectedContainer.Networks = append(inspectedContainer.Networks, containers.InspectedContainerNetwork{
			Id:         networkIDs[networkName],
			Name:       networkName,
			IPAddress:  rawNetwork.IPAddress,
			MacAddress: rawNetwork.MacAddress,
			Gateway:    rawNetwork.Gateway,
			Aliases:    rawNetwork.Aliases,
		})
	}

	return inspectedContainer, nil
}

func (wco *WslcCliOrchestrator) StartContainers(ctx context.Context, options containers.StartContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}
	defer closeWriteCloser(options.StdOutStream)
	defer closeWriteCloser(options.StdErrStream)

	return runSequentially(ctx, "containers", options.Containers, func(containerReference string) error {
		cmd := makeWslcCommand("container", "start", containerReference)
		_, errBuf, runErr := wco.runBufferedWslcCommandInternal(
			ctx,
			"StartContainer",
			cmd,
			options.StdOutStream,
			options.StdErrStream,
			ordinaryCommandTimeout,
			false,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, containerNotFoundMatch))
		}
		return nil
	})
}

func (wco *WslcCliOrchestrator) StopContainers(ctx context.Context, options containers.StopContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	timeout := ordinaryCommandTimeout
	stopArgs := []string{"container", "stop"}
	if options.SecondsToKill > 0 {
		stopArgs = append(stopArgs, "-t", fmt.Sprintf("%d", options.SecondsToKill))
		gracePeriod := time.Duration(options.SecondsToKill) * time.Second
		if gracePeriod < 0 {
			return nil, fmt.Errorf("container stop timeout is too large")
		}
		timeout += gracePeriod
	}

	return runSequentially(ctx, "containers", options.Containers, func(containerReference string) error {
		args := append(append([]string{}, stopArgs...), containerReference)
		cmd := makeWslcCommand(args...)
		_, errBuf, runErr := wco.runBufferedWslcCommand(
			ctx,
			"StopContainer",
			cmd,
			nil,
			nil,
			timeout,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, containerNotFoundMatch))
		}
		return nil
	})
}

func (wco *WslcCliOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	return runSequentially(ctx, "containers", options.Containers, func(containerReference string) error {
		args := []string{"container", "remove", "--volumes"}
		if options.Force {
			args = append(args, "--force")
		}
		args = append(args, containerReference)

		cmd := makeWslcCommand(args...)
		_, errBuf, runErr := wco.runBufferedWslcCommand(
			ctx,
			"RemoveContainer",
			cmd,
			nil,
			nil,
			ordinaryCommandTimeout,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, containerNotFoundMatch, objectInUseMatch))
		}
		return nil
	})
}

func runSequentially(
	ctx context.Context,
	objectKind string,
	references []string,
	run func(reference string) error,
) ([]string, error) {
	successes := make([]string, 0, len(references))
	var operationErrors error

	for _, reference := range references {
		if contextErr := ctx.Err(); contextErr != nil {
			operationErrors = errors.Join(operationErrors, contextErr)
			break
		}
		if reference == "" {
			operationErrors = errors.Join(operationErrors, fmt.Errorf("%s reference cannot be empty", objectKind))
			continue
		}

		if runErr := run(reference); runErr != nil {
			operationErrors = errors.Join(operationErrors, fmt.Errorf("processing %s %q: %w", objectKind, reference, runErr))
			continue
		}
		successes = append(successes, reference)
	}

	return successes, errors.Join(
		operationErrors,
		incompleteError(objectKind, len(successes), len(references)),
	)
}

func (wco *WslcCliOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if options.Container == "" {
		return nil, fmt.Errorf("must specify a container")
	}
	if options.Command == "" {
		return nil, fmt.Errorf("must specify a command")
	}

	args := []string{"container", "exec"}
	if options.WorkingDirectory != "" {
		args = append(args, "--workdir", options.WorkingDirectory)
	}
	for _, envVar := range options.Env {
		if envVar.Name == "" {
			return nil, fmt.Errorf("container exec environment variable name cannot be empty")
		}
		args = append(args, "--env", envVar.Name+"="+envVar.Value)
	}
	for _, envFile := range options.EnvFiles {
		if envFile == "" {
			return nil, fmt.Errorf("container exec environment file path cannot be empty")
		}
		args = append(args, "--env-file", envFile)
	}
	args = append(args, options.Container, options.Command)
	args = append(args, options.Args...)

	cmd := makeWslcCommand(args...)
	cmd.Stdout = options.StdOutStream
	cmd.Stderr = options.StdErrStream

	exitCodes := make(chan int32, 1)
	exitHandler := process.ProcessExitHandlerFunc(func(_ process.Pid_t, exitCode int32, exitErr error) {
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) && !errors.Is(exitErr, context.DeadlineExceeded) {
			wco.log.Error(exitErr, "WSLC container exec command failed", "Container", options.Container)
		}
		exitCodes <- exitCode
		close(exitCodes)
	})

	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	_, startWaitForExit, startErr := wco.executor.StartProcess(
		ctx,
		cmd,
		exitHandler,
		process.CreationFlagEnsureKillOnDispose,
		nil,
	)
	if startErr != nil {
		return nil, fmt.Errorf("failed to start WSLC container exec command: %w", startErr)
	}
	startWaitForExit()
	return exitCodes, nil
}

func (wco *WslcCliOrchestrator) AttachContainer(
	ctx context.Context,
	options containers.AttachContainerOptions,
) (*termpty.PseudoTerminalProcess, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if options.Container == "" {
		return nil, fmt.Errorf("must specify a container")
	}

	cmd := makeWslcCommand("container", "attach", options.Container)
	return termpty.StartProcessWithTerminal(ctx, wco.executor, &termpty.CommandSpec{
		Cmd:           cmd,
		CreationFlags: process.CreationFlagEnsureKillOnDispose,
		Cols:          options.Cols,
		Rows:          options.Rows,
	})
}

func (wco *WslcCliOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	if options.Container == "" {
		return fmt.Errorf("must specify a container")
	}
	if len(options.Entries) == 0 {
		return fmt.Errorf("must specify at least one file-system entry")
	}

	archive, archiveErr := containers.CreateFilesArchive(ctx, wco.log, options)
	if archiveErr != nil {
		return archiveErr
	}
	if archive == nil {
		return nil
	}

	cmd := makeWslcCommand(
		"container",
		"cp",
		"-a=false",
		"-",
		options.Container+":/",
	)
	cmd.Stdin = archive
	_, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"CreateFiles",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return errors.Join(runErr, normalizeCliErrors(errBuf, containerNotFoundMatch))
	}
	return nil
}

func (wco *WslcCliOrchestrator) CaptureContainerLogs(
	ctx context.Context,
	containerReference string,
	stdout usvc_io.WriteSyncerCloser,
	stderr usvc_io.WriteSyncerCloser,
	options containers.StreamContainerLogsOptions,
) error {
	if containerReference == "" {
		return fmt.Errorf("must specify a container")
	}
	if stdout == nil || stderr == nil {
		return fmt.Errorf("container log destinations cannot be nil")
	}

	args := options.Apply([]string{"container", "logs"})
	args = append(args, containerReference)
	cmd := makeWslcCommand(args...)

	exitHandler, startErr := wco.startStreamingWslcCommand(
		ctx,
		"CaptureContainerLogs",
		cmd,
		stdout,
		stderr,
	)
	if startErr != nil {
		closeLogDestination(wco, containerReference, "stdout", stdout)
		closeLogDestination(wco, containerReference, "stderr", stderr)
		return startErr
	}

	go func() {
		<-exitHandler.Exited()
		exitInfo := exitHandler.ExitInfo()
		if exitInfo.Err != nil &&
			!errors.Is(exitInfo.Err, context.Canceled) &&
			!errors.Is(exitInfo.Err, context.DeadlineExceeded) {
			wco.log.Error(exitInfo.Err, "Capturing WSLC container logs failed", "Container", containerReference)
		} else if exitInfo.ExitCode != 0 {
			wco.log.Error(
				fmt.Errorf("wslc logs command exited with code %d", exitInfo.ExitCode),
				"Capturing WSLC container logs failed",
				"Container",
				containerReference,
			)
		}

		closeLogDestination(wco, containerReference, "stdout", stdout)
		closeLogDestination(wco, containerReference, "stderr", stderr)
	}()

	return nil
}

func closeLogDestination(
	wco *WslcCliOrchestrator,
	containerReference string,
	streamName string,
	destination usvc_io.WriteSyncerCloser,
) {
	if closeErr := destination.Close(); closeErr != nil {
		wco.log.Error(closeErr, "Closing container log destination failed", "Container", containerReference, "Stream", streamName)
	}
}
