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

	"github.com/microsoft/dcp/internal/containers"
)

func (wco *WslcCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	if options.Name == "" {
		return "", fmt.Errorf("must specify a network name")
	}
	if options.IPv6 {
		return "", fmt.Errorf("wslc does not support enabling IPv6 when creating a network")
	}

	args := []string{"network", "create"}
	labelKeys := make([]string, 0, len(options.Labels))
	for key := range options.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		if key == "" {
			return "", fmt.Errorf("network label key cannot be empty")
		}
		args = append(args, "--label", key+"="+options.Labels[key])
	}
	args = append(args, options.Name)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"CreateNetwork",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return "", errors.Join(
			runErr,
			normalizeCliErrors(errBuf, networkNotFoundMatch, alreadyExistsMatch, allocationFailureMatch),
		)
	}

	outputName, outputErr := parseSingleIdentifier(outBuf)
	if outputErr == nil && outputName != options.Name {
		outputErr = fmt.Errorf("wslc network create returned name %q instead of %q", outputName, options.Name)
	}

	createdNetwork, inspectErr := wco.resolveNetwork(ctx, options.Name)
	if inspectErr != nil {
		return "", errors.Join(outputErr, fmt.Errorf("inspecting newly created WSLC network %q: %w", options.Name, inspectErr))
	}
	if createdNetwork.Id == "" {
		return "", errors.Join(outputErr, fmt.Errorf("newly created WSLC network %q did not report an ID", options.Name))
	}
	return createdNetwork.Id, outputErr
}

func (wco *WslcCliOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	return runSequentially(ctx, "networks", options.Networks, func(networkReference string) error {
		network, resolveErr := wco.resolveNetwork(ctx, networkReference)
		if resolveErr != nil {
			return resolveErr
		}

		args := []string{"network", "remove"}
		if options.Force {
			args = append(args, "--force")
		}
		args = append(args, network.Name)

		cmd := makeWslcCommand(args...)
		_, errBuf, runErr := wco.runBufferedWslcCommand(
			ctx,
			"RemoveNetwork",
			cmd,
			nil,
			nil,
			ordinaryCommandTimeout,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, networkNotFoundMatch, objectInUseMatch))
		}
		return nil
	})
}

func (wco *WslcCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	rawNetworks, inspectErr := wco.inspectNetworksRaw(ctx, options.Networks)
	inspectedNetworks := make([]containers.InspectedNetwork, 0, len(rawNetworks))
	var conversionErr error
	for _, rawNetwork := range rawNetworks {
		inspectedNetwork, convertErr := convertInspectedNetwork(rawNetwork)
		if convertErr != nil {
			conversionErr = errors.Join(conversionErr, containers.ErrUnmarshalling, convertErr)
			continue
		}
		inspectedNetworks = append(inspectedNetworks, inspectedNetwork)
	}

	return inspectedNetworks, errors.Join(
		inspectErr,
		conversionErr,
		incompleteError("networks", len(inspectedNetworks), len(options.Networks)),
	)
}

func (wco *WslcCliOrchestrator) inspectNetworksRaw(
	ctx context.Context,
	networkReferences []string,
) ([]wslcInspectedNetwork, error) {
	args := append([]string{"network", "inspect", "--format", "json"}, networkReferences...)
	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"InspectNetworks",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)

	rawNetworks, decodeErr := decodeJSONArray[wslcInspectedNetwork](outBuf)
	if runErr != nil {
		runErr = errors.Join(
			runErr,
			normalizeCliErrors(errBuf, networkNotFoundMatch.MaxObjects(len(networkReferences))),
		)
	}
	return rawNetworks, errors.Join(runErr, decodeErr)
}

func convertInspectedNetwork(rawNetwork wslcInspectedNetwork) (containers.InspectedNetwork, error) {
	networkID := strings.TrimSpace(rawNetwork.ID)
	networkName := strings.TrimSpace(rawNetwork.Name)
	if networkID == "" {
		return containers.InspectedNetwork{}, fmt.Errorf("inspected WSLC network did not contain an ID")
	}
	if networkName == "" {
		return containers.InspectedNetwork{}, fmt.Errorf("inspected WSLC network %q did not contain a name", networkID)
	}

	inspectedNetwork := containers.InspectedNetwork{
		Name:       networkName,
		Id:         networkID,
		Driver:     rawNetwork.Driver,
		Labels:     rawNetwork.Labels,
		Scope:      rawNetwork.Scope,
		IPv6:       bool(rawNetwork.EnableIPv6) || bool(rawNetwork.IPv6),
		Internal:   bool(rawNetwork.Internal),
		Attachable: bool(rawNetwork.Attachable),
		Ingress:    bool(rawNetwork.Ingress),
		CreatedAt:  rawNetwork.Created.Time,
	}
	for _, config := range rawNetwork.IPAM.Config {
		if config.Subnet != "" {
			inspectedNetwork.Subnets = append(inspectedNetwork.Subnets, config.Subnet)
		}
		if config.Gateway != "" {
			inspectedNetwork.Gateways = append(inspectedNetwork.Gateways, config.Gateway)
		}
	}

	containerIDs := make([]string, 0, len(rawNetwork.Containers))
	for containerID := range rawNetwork.Containers {
		containerIDs = append(containerIDs, containerID)
	}
	sort.Strings(containerIDs)
	for _, containerID := range containerIDs {
		inspectedNetwork.Containers = append(inspectedNetwork.Containers, containers.InspectedNetworkContainer{
			Id:   containerID,
			Name: rawNetwork.Containers[containerID].Name,
		})
	}

	return inspectedNetwork, nil
}

func (wco *WslcCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	if options.Container == "" {
		return fmt.Errorf("must specify a container")
	}
	network, resolveErr := wco.resolveNetwork(ctx, options.Network)
	if resolveErr != nil {
		return resolveErr
	}

	args := []string{"network", "connect"}
	for _, alias := range options.Aliases {
		if alias == "" {
			return fmt.Errorf("network alias cannot be empty")
		}
		args = append(args, "--network-alias", alias)
	}
	args = append(args, network.Name, options.Container)

	cmd := makeWslcCommand(args...)
	_, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"ConnectNetwork",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return errors.Join(
			runErr,
			normalizeCliErrors(errBuf, containerNotFoundMatch, networkNotFoundMatch, alreadyExistsMatch),
		)
	}
	return nil
}

func (wco *WslcCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	if options.Container == "" {
		return fmt.Errorf("must specify a container")
	}
	network, resolveErr := wco.resolveNetwork(ctx, options.Network)
	if resolveErr != nil {
		return resolveErr
	}

	cmd := makeWslcCommand("network", "disconnect", network.Name, options.Container)
	_, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"DisconnectNetwork",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		runErr = errors.Join(runErr, normalizeCliErrors(errBuf, containerNotFoundMatch, networkNotFoundMatch))
	}

	if !options.Force {
		return runErr
	}

	verifyErr := wco.verifyNetworkDetached(ctx, network, options.Container)
	if verifyErr == nil {
		return nil
	}
	return errors.Join(runErr, verifyErr)
}

func (wco *WslcCliOrchestrator) verifyNetworkDetached(
	ctx context.Context,
	network containers.InspectedNetwork,
	containerReference string,
) error {
	rawContainers, containerInspectErr := wco.inspectContainersRaw(ctx, []string{containerReference})

	containerID := containerReference
	containerName := containerReference
	switch len(rawContainers) {
	case 1:
		if containerInspectErr != nil {
			return fmt.Errorf(
				"verifying WSLC container network configuration returned an object with an error: %w",
				containerInspectErr,
			)
		}
		rawContainer := rawContainers[0]
		containerID = strings.TrimSpace(rawContainer.ID)
		if containerID == "" {
			return fmt.Errorf(
				"wslc could not verify forced disconnect: container %q inspection returned an empty ID",
				containerReference,
			)
		}
		containerName = strings.TrimPrefix(strings.TrimSpace(rawContainer.Name), "/")
		if containerName == "" {
			return fmt.Errorf(
				"wslc could not verify forced disconnect: container %q inspection returned an empty name",
				containerReference,
			)
		}
		if _, attached := rawContainer.NetworkSettings.Networks[network.Name]; attached {
			return fmt.Errorf(
				"wslc could not verify forced disconnect: network %q remains in container %q configuration",
				network.Name,
				containerReference,
			)
		}
	case 0:
		if !isTrustworthyNotFound(containerInspectErr) {
			return errors.Join(
				containerInspectErr,
				fmt.Errorf(
					"wslc could not verify forced disconnect: container %q inspection returned no object",
					containerReference,
				),
			)
		}
	default:
		return errors.Join(
			containerInspectErr,
			fmt.Errorf(
				"wslc could not verify forced disconnect: container %q inspection returned %d objects",
				containerReference,
				len(rawContainers),
			),
		)
	}

	rawNetworks, networkInspectErr := wco.inspectNetworksRaw(ctx, []string{network.Name})
	if len(rawNetworks) == 0 {
		if isTrustworthyNotFound(networkInspectErr) {
			return nil
		}
		return errors.Join(
			networkInspectErr,
			fmt.Errorf(
				"wslc could not verify forced disconnect: network %q inspection returned no object",
				network.Name,
			),
		)
	}
	if len(rawNetworks) != 1 {
		return errors.Join(
			networkInspectErr,
			fmt.Errorf(
				"wslc could not verify forced disconnect: network %q inspection returned %d objects",
				network.Name,
				len(rawNetworks),
			),
		)
	}
	if networkInspectErr != nil {
		return fmt.Errorf("verifying WSLC network endpoints: %w", networkInspectErr)
	}
	postNetworkID := strings.TrimSpace(rawNetworks[0].ID)
	if postNetworkID == "" {
		return fmt.Errorf(
			"wslc could not verify forced disconnect: network %q inspection returned an empty ID",
			network.Name,
		)
	}
	if postNetworkID != network.Id {
		return fmt.Errorf(
			"wslc could not verify forced disconnect: network %q identity changed from %q to %q",
			network.Name,
			network.Id,
			postNetworkID,
		)
	}
	postNetworkName := strings.TrimSpace(rawNetworks[0].Name)
	if postNetworkName == "" {
		return fmt.Errorf(
			"wslc could not verify forced disconnect: network %q inspection returned an empty name",
			network.Name,
		)
	}
	if postNetworkName != network.Name {
		return fmt.Errorf(
			"wslc could not verify forced disconnect: network %q inspection returned name %q",
			network.Name,
			postNetworkName,
		)
	}

	for endpointID, endpoint := range rawNetworks[0].Containers {
		if identifiersMatch(endpointID, containerID) ||
			endpoint.Name == containerName ||
			endpoint.Name == containerReference {
			return fmt.Errorf(
				"wslc could not verify forced disconnect: network %q still reports an active endpoint for container %q",
				network.Name,
				containerReference,
			)
		}
	}
	return nil
}

func isTrustworthyNotFound(err error) bool {
	if err == nil || !errors.Is(err, containers.ErrNotFound) {
		return false
	}

	untrustworthyErrors := []error{
		context.Canceled,
		context.DeadlineExceeded,
		containers.ErrUnmatched,
		containers.ErrUnmarshalling,
		containers.ErrIncomplete,
		containers.ErrRuntimeNotHealthy,
		containers.ErrAlreadyExists,
		containers.ErrCouldNotAllocate,
		containers.ErrObjectInUse,
	}
	for _, untrustworthyErr := range untrustworthyErrors {
		if errors.Is(err, untrustworthyErr) {
			return false
		}
	}
	return true
}

func identifiersMatch(first string, second string) bool {
	if first == "" || second == "" {
		return false
	}
	if first == second {
		return true
	}
	const minimumPrefixLength = 12
	return len(first) >= minimumPrefixLength &&
		len(second) >= minimumPrefixLength &&
		(strings.HasPrefix(first, second) || strings.HasPrefix(second, first))
}

func (wco *WslcCliOrchestrator) ListNetworks(ctx context.Context, options containers.ListNetworksOptions) ([]containers.ListedNetwork, error) {
	args := []string{"network", "list", "--no-trunc"}
	for _, label := range options.Filters.LabelFilters {
		filter := "label=" + label.Key
		if label.Value != "" {
			filter += "=" + label.Value
		}
		args = append(args, "--filter", filter)
	}
	args = append(args, "--format", "json")

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"ListNetworks",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return nil, errors.Join(runErr, normalizeCliErrors(errBuf))
	}

	rawNetworks, decodeErr := decodeJSONLines[wslcListedNetwork](outBuf)
	listedNetworks := make([]containers.ListedNetwork, 0, len(rawNetworks))
	labelInspectionIDs := make([]string, 0, len(rawNetworks))
	for _, rawNetwork := range rawNetworks {
		if rawNetwork.ID == "" || rawNetwork.Name == "" {
			decodeErr = errors.Join(
				decodeErr,
				containers.ErrUnmarshalling,
				fmt.Errorf("listed WSLC network did not contain both an ID and name"),
			)
			continue
		}
		listedNetworks = append(listedNetworks, containers.ListedNetwork{
			Driver:   rawNetwork.Driver,
			ID:       rawNetwork.ID,
			IPv6:     bool(rawNetwork.IPv6),
			Internal: bool(rawNetwork.Internal),
			Name:     rawNetwork.Name,
		})
		labelInspectionIDs = append(labelInspectionIDs, rawNetwork.ID)
	}

	if len(labelInspectionIDs) > 0 {
		inspectedNetworks, inspectErr := wco.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: labelInspectionIDs,
		})
		labelsByID := make(map[string]map[string]string, len(inspectedNetworks))
		for _, inspectedNetwork := range inspectedNetworks {
			labelsByID[inspectedNetwork.Id] = inspectedNetwork.Labels
		}
		for index := range listedNetworks {
			listedNetworks[index].Labels = labelsByID[listedNetworks[index].ID]
		}
		if inspectErr != nil || len(inspectedNetworks) < len(labelInspectionIDs) {
			decodeErr = errors.Join(
				decodeErr,
				fmt.Errorf("resolving authoritative labels for listed WSLC networks: %w",
					errors.Join(inspectErr, incompleteError("networks", len(inspectedNetworks), len(labelInspectionIDs)))),
			)
		}
	}

	return listedNetworks, decodeErr
}

func (wco *WslcCliOrchestrator) resolveNetwork(
	ctx context.Context,
	networkReference string,
) (containers.InspectedNetwork, error) {
	if networkReference == "" {
		return containers.InspectedNetwork{}, fmt.Errorf("must specify a network")
	}

	inspectedNetworks, inspectErr := wco.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{networkReference},
	})
	if inspectErr != nil {
		return containers.InspectedNetwork{}, inspectErr
	}
	if len(inspectedNetworks) != 1 {
		return containers.InspectedNetwork{}, fmt.Errorf(
			"wslc network lookup for %q returned %d objects",
			networkReference,
			len(inspectedNetworks),
		)
	}
	return inspectedNetworks[0], nil
}

func (*WslcCliOrchestrator) DefaultNetworkName() string {
	return "bridge"
}

func (*WslcCliOrchestrator) IsBuiltInNetwork(networkName string) bool {
	return networkName == "bridge" || networkName == "host" || networkName == "none"
}
