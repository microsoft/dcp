// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	std_slices "slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	volumeInspectionTimeout = 6 * time.Second
)

type containerID string

// Container orchestrators, especially Docker, can be flakey (report errors that are not real problems).
// See https://github.com/dotnet/aspire/issues/5109 for customer report that led us to start using retries when calling the orchestrator.

// Default exponential backoff for calling container orchestrator, with 30 seconds timeout.
// Compare with ordinaryDockerCommandTimeout and ordinaryPodmanCommandTimeout.
func defaultContainerOrchestratorBackoff() *backoff.ExponentialBackOff {
	return exponentialBackoff(30 * time.Second)
}

func exponentialBackoff(timeout time.Duration) *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(200*time.Millisecond),
		backoff.WithMultiplier(2),
		backoff.WithRandomizationFactor(0.1),
		backoff.WithMaxElapsedTime(timeout),
	)
}

func callWithRetryAndVerification[RT any](
	callContext context.Context,
	b backoff.BackOff,
	action func(context.Context) error,
	verify func(context.Context) (RT, error),
) (RT, error) {
	actAndVerify := func() (RT, error) {
		if callContext.Err() != nil {
			return *new(RT), resiliency.Permanent(callContext.Err())
		}

		actionErr := action(callContext)
		var permanentErr *backoff.PermanentError
		if errors.As(actionErr, &permanentErr) {
			return *new(RT), actionErr
		}

		// Unfortunately, an error from the action does not NECESSARILY mean the action failed.

		// Take a short break before verifying the action.
		time.Sleep(200 * time.Millisecond)

		res, verifyErr := verify(callContext)
		if verifyErr != nil {
			return *new(RT), resiliency.Join(verifyErr, actionErr)
		}

		return res, nil
	}

	return resiliency.RetryGet(callContext, b, actAndVerify)
}

func inspectContainer(findContext context.Context, o containers.ContainerOrchestrator, containerID string) (*containers.InspectedContainer, error) {
	b := exponentialBackoff(containerInspectionTimeout)
	return resiliency.RetryGet(findContext, b, func() (*containers.InspectedContainer, error) {
		inspectedCtrs, err := o.InspectContainers(findContext, containers.InspectContainersOptions{Containers: []string{containerID}})
		if err != nil {
			return nil, err
		}
		if len(inspectedCtrs) == 0 {
			return nil, containers.ErrNotFound
		}
		return &inspectedCtrs[0], nil
	})
}

func inspectContainerIfExists(findContext context.Context, o containers.ContainerOrchestrator, containerID string) (*containers.InspectedContainer, error) {
	b := exponentialBackoff(containerInspectionTimeout)
	return resiliency.RetryGet(findContext, b, func() (*containers.InspectedContainer, error) {
		inspectedCtrs, err := o.InspectContainers(findContext, containers.InspectContainersOptions{Containers: []string{containerID}})
		if errors.Is(err, containers.ErrNotFound) {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		} else if err != nil {
			return nil, err
		}

		if len(inspectedCtrs) == 0 {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		}

		return &inspectedCtrs[0], nil
	})
}

func verifyContainerState(
	ctx context.Context,
	o containers.ContainerOrchestrator,
	containerID string,
	isInState func(*containers.InspectedContainer) error,
) (*containers.InspectedContainer, error) {
	inspected, inspectErr := inspectContainer(ctx, o, containerID)
	if inspectErr != nil {
		return nil, inspectErr
	}

	if err := isInState(inspected); err != nil {
		return inspected, err
	} else {
		return inspected, nil
	}
}

func verifyNetworkState(
	ctx context.Context,
	o containers.ContainerOrchestrator,
	network string,
	isInState func(*containers.InspectedNetwork) error,
) (*containers.InspectedNetwork, error) {
	inspected, inspectErr := inspectNetwork(ctx, o, network)
	if inspectErr != nil {
		return nil, inspectErr
	}

	if err := isInState(inspected); err != nil {
		return inspected, err
	} else {
		return inspected, nil
	}
}

func stopContainer(stopContext context.Context, o containers.ContainerOrchestrator, containerID string) error {
	action := func(ctx context.Context) error {
		_, stopErr := o.StopContainers(ctx, containers.StopContainersOptions{
			Containers:    []string{containerID},
			SecondsToKill: stopContainerTimeoutSeconds,
		})

		return stopErr
	}

	verify := func(ctx context.Context) (any, error) {
		_, verifyErr := verifyContainerState(ctx, o, containerID, func(i *containers.InspectedContainer) error {
			if i.Status == containers.ContainerStatusExited {
				return nil
			} else {
				return fmt.Errorf("status of container %s is '%s' (was expecting '%s')", containerID, i.Status, containers.ContainerStatusExited)
			}
		})

		if errors.Is(verifyErr, containers.ErrNotFound) {
			return nil, nil // Special case: treat missing container as "stopped"
		}

		return nil, verifyErr
	}

	_, stopErr := callWithRetryAndVerification(stopContext, defaultContainerOrchestratorBackoff(), action, verify)
	return stopErr
}

func removeContainer(removeContext context.Context, o containers.ContainerOrchestrator, containerID string) error {
	action := func(ctx context.Context) error {
		_, removeErr := o.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})

		return removeErr
	}

	verify := func(ctx context.Context) (any, error) {
		// Do not use r.inspectContainer() here, we do not want to retry this when the container is not found
		_, inspectErr := o.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerID}})

		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil, nil // This is what we wanted, the container is gone
		}

		if inspectErr != nil {
			return nil, inspectErr
		} else {
			return nil, fmt.Errorf("container %s still exists after remove", containerID)
		}
	}

	_, removeErr := callWithRetryAndVerification(removeContext, defaultContainerOrchestratorBackoff(), action, verify)
	return removeErr
}

func createContainer(createCtx context.Context, o containers.ContainerOrchestrator, creationOptions containers.CreateContainerOptions) (*containers.InspectedContainer, error) {
	creationOptions.Timeout = 10 * time.Minute // Compare to defaultCreateContainerTimeout
	var containerID string
	const maxRetries = 2 // We will retry overall operation at most twice since it has a long timeout

	action := func(ctx context.Context) error {
		var createErr error
		// There are errors that can still result in a valid container ID, so we need to store it if one was returned
		var maybeID string
		maybeID, createErr = o.CreateContainer(ctx, creationOptions)
		maybeID = strings.TrimSpace(maybeID)
		if maybeID != "" {
			containerID = maybeID
		}
		return createErr
	}

	verify := func(ctx context.Context) (*containers.InspectedContainer, error) {
		var effectiveContainerID string
		if containerID != "" {
			effectiveContainerID = containerID
		} else {
			effectiveContainerID = creationOptions.Name
		}

		inspected, inspectErr := inspectContainer(createCtx, o, effectiveContainerID)
		return inspected, inspectErr
	}

	b := backoff.WithMaxRetries(exponentialBackoff(creationOptions.Timeout), maxRetries)
	inspected, err := callWithRetryAndVerification(createCtx, b, action, verify)
	return inspected, err
}

func startContainer(
	startCtx context.Context,
	o containers.ContainerOrchestrator,
	containerName string,
	containerID string,
	streamOptions containers.StreamCommandOptions,
) (*containers.InspectedContainer, error) {
	action := func(ctx context.Context) error {
		_, startErr := o.StartContainers(ctx, containers.StartContainersOptions{
			Containers:           []string{containerID},
			StreamCommandOptions: streamOptions,
		})
		return startErr
	}

	verify := func(ctx context.Context) (*containers.InspectedContainer, error) {
		return verifyContainerState(ctx, o, containerID, func(i *containers.InspectedContainer) error {
			switch i.Status {
			case containers.ContainerStatusRunning:
				return nil

			case containers.ContainerStatusDead:
				return resiliency.Permanent(fmt.Errorf("container '%s' start failed (current state is 'dead')", containerName))

			case containers.ContainerStatusExited:
				// It is possible that the container starts and then exits very quickly afterwards.
				// For the sake of determining whether the startup was successful, we will assume that it was if exit code == 0.
				if i.ExitCode == 0 {
					return nil
				} else {
					return resiliency.Permanent(fmt.Errorf("container '%s' start failed (exit code %d)", containerName, i.ExitCode))
				}

			default:
				errMsg := fmt.Sprintf("status of container '%s' is '%s' (was expecting '%s')", containerName, i.Status, containers.ContainerStatusRunning)
				if i.Error != "" {
					errMsg += fmt.Sprintf(", error: %s", i.Error)
				}
				return errors.New(errMsg)
			}
		})
	}

	inspected, err := callWithRetryAndVerification(startCtx, defaultContainerOrchestratorBackoff(), action, verify)
	return inspected, err
}

func disconnectNetwork(ctx context.Context, o containers.ContainerOrchestrator, opts containers.DisconnectNetworkOptions) error {
	action := func(ctx context.Context) error {
		return o.DisconnectNetwork(ctx, opts)
	}

	verify := func(ctx context.Context) (*containers.InspectedNetwork, error) {
		return verifyNetworkState(ctx, o, opts.Network, func(i *containers.InspectedNetwork) error {
			if !std_slices.ContainsFunc(i.Containers, func(c containers.InspectedNetworkContainer) bool {
				return c.Name == opts.Container || strings.HasPrefix(c.Id, opts.Container)
			}) {
				return nil
			}
			return fmt.Errorf("container %s is still connected to network %s", opts.Container, opts.Network)
		})
	}

	_, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return err
}

func inspectNetwork(ctx context.Context, o containers.NetworkOrchestrator, network string) (*containers.InspectedNetwork, error) {
	b := exponentialBackoff(networkInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() (*containers.InspectedNetwork, error) {
		networks, err := o.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{network}})
		if err != nil {
			return nil, err
		}
		if len(networks) == 0 {
			return nil, containers.ErrNotFound
		}
		return &networks[0], nil
	})
}

func inspectNetworkIfExists(ctx context.Context, o containers.ContainerOrchestrator, network string) (*containers.InspectedNetwork, error) {
	b := exponentialBackoff(networkInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() (*containers.InspectedNetwork, error) {
		networks, err := o.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{network}})
		if errors.Is(err, containers.ErrNotFound) {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		} else if err != nil {
			return nil, err
		}

		if len(networks) == 0 {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		}

		return &networks[0], nil
	})
}

func createNetwork(ctx context.Context, o containers.NetworkOrchestrator, opts containers.CreateNetworkOptions) (*containers.InspectedNetwork, error) {
	action := func(ctx context.Context) error {
		_, err := o.CreateNetwork(ctx, opts)

		// Stop retrying if the network already exists or the address pool is exhausted,
		// as these are not transient errors.
		if errors.Is(err, containers.ErrAlreadyExists) || errors.Is(err, containers.ErrCouldNotAllocate) {
			return backoff.Permanent(err)
		}

		return err
	}

	verify := func(ctx context.Context) (*containers.InspectedNetwork, error) {
		return inspectNetwork(ctx, o, opts.Name)
	}

	inspected, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return inspected, err
}

func removeNetwork(ctx context.Context, o containers.NetworkOrchestrator, networkID string, log logr.Logger) error {
	action := func(ctx context.Context) error {
		_, err := o.RemoveNetworks(ctx, containers.RemoveNetworksOptions{Networks: []string{networkID}, Force: true})
		if err != nil {
			// Network removal has been particularly problematic in the past, so we want extra logging.
			log.V(1).Info("Container network could not be removed", "network", networkID, "error", err.Error())
		}
		return err
	}

	verify := func(ctx context.Context) (any, error) {
		// Do not use r.inspectNetwork() here, we do not want to retry this when the network is not found
		_, inspectErr := o.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})

		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil, nil // Network is gone as expected
		}

		if inspectErr != nil {
			return nil, inspectErr
		} else {
			return nil, fmt.Errorf("network %s still exists", networkID)
		}
	}

	_, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return err
}

func inspectContainerVolume(ctx context.Context, o containers.VolumeOrchestrator, volumeName string) (*containers.InspectedVolume, error) {
	b := exponentialBackoff(volumeInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() (*containers.InspectedVolume, error) {
		inspectedVolumes, err := o.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
		if err != nil {
			return nil, err
		}
		if len(inspectedVolumes) == 0 {
			return nil, containers.ErrNotFound
		}
		return &inspectedVolumes[0], nil
	})
}

func inspectContainerVolumeIfExists(ctx context.Context, o containers.VolumeOrchestrator, volumeName string) (*containers.InspectedVolume, error) {
	b := exponentialBackoff(volumeInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() (*containers.InspectedVolume, error) {
		inspectedVolumes, err := o.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
		if errors.Is(err, containers.ErrNotFound) {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		} else if err != nil {
			return nil, err
		}

		if len(inspectedVolumes) == 0 {
			return nil, resiliency.Permanent(containers.ErrNotFound)
		}

		return &inspectedVolumes[0], nil
	})
}

func removeVolume(ctx context.Context, o containers.VolumeOrchestrator, volumeName string) error {
	action := func(ctx context.Context) error {
		_, err := o.RemoveVolumes(ctx, containers.RemoveVolumesOptions{Volumes: []string{volumeName}})
		if errors.Is(err, containers.ErrObjectInUse) {
			// Treat this error as permanent and let the caller decide how to handle it
			// (e.g. wait, or retry, or try to remove containers that use the volume).
			return backoff.Permanent(err)
		}
		return err
	}

	verify := func(ctx context.Context) (any, error) {
		// Do not use r.inspectContainerVolume() here, we do not want to retry this when the volume is not found
		_, inspectErr := o.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})

		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil, nil // Volume is gone as expected
		}

		if inspectErr != nil {
			return nil, inspectErr
		} else {
			return nil, fmt.Errorf("volume %s still exists", volumeName)
		}
	}

	_, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return err
}

func createVolume(ctx context.Context, o containers.VolumeOrchestrator, volumeName string) (*containers.InspectedVolume, error) {
	action := func(ctx context.Context) error {
		err := o.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volumeName})

		if errors.Is(err, containers.ErrAlreadyExists) {
			return backoff.Permanent(err)
		}

		return err
	}

	verify := func(ctx context.Context) (*containers.InspectedVolume, error) {
		return inspectContainerVolume(ctx, o, volumeName)
	}

	inspected, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return inspected, err
}

// Returns the host address and port for the given service producer port.
// The spec is checked for a matching apiv1.ContainerPort in the spec, first by matching on apiv1.ContainerPort.HostPort,
// and if unsuccessful, by matching on apiv1.ContainerPort.ContainerPort.
// If a matching ContainerPort is found in the spec, and that matching ContainerPort has a HostPort property set
// (i.e. host port is not auto-assigned), the host address and port in the spec are returned.
// Otherwise the function takes the inspected container and searches for port config that matches the service producer port
// (on the container side). If found, corresponding host port and host IP are returned.
func getHostAddressAndPortForContainerPort(
	ctrSpec apiv1.ContainerSpec,
	serviceProducerPort int32,
	inspected *containers.InspectedContainer,
	log logr.Logger,
) (string, int32, error) {
	var matchedPort apiv1.ContainerPort
	found := false

	matchedByHost := slices.Select(ctrSpec.Ports, func(p apiv1.ContainerPort) bool {
		return p.HostPort == serviceProducerPort
	})
	if len(matchedByHost) > 0 {
		matchedPort = matchedByHost[0]
		found = true
	} else {
		matchedByContainer := slices.Select(ctrSpec.Ports, func(p apiv1.ContainerPort) bool {
			return p.ContainerPort == serviceProducerPort
		})
		if len(matchedByContainer) > 0 {
			matchedPort = matchedByContainer[0]
			found = true
		}
	}

	if found && matchedPort.HostPort != 0 {
		hostAddress := normalizeHostAddress(matchedPort.HostIP)
		// If the spec contains a port matching the desired container port, just use that
		log.V(1).Info("Found matching port in Container spec", "ServiceProducerPort", serviceProducerPort, "HostPort", matchedPort.HostPort, "HostIP", matchedPort.HostIP, "EffectiveHostAddress", hostAddress)
		return hostAddress, matchedPort.HostPort, nil
	}

	if inspected.Status != containers.ContainerStatusRunning {
		return "", 0, fmt.Errorf("container '%s' is not running: %s", inspected.Name, inspected.Status)
	}

	var matchedHostPort containers.InspectedContainerHostPortConfig
	found = false
	for k, v := range inspected.Ports {
		ctrPort := strings.Split(k, "/")[0]

		if ctrPort == fmt.Sprintf("%d", serviceProducerPort) {
			matchedHostPort = v[0]
			found = true
			break
		}
	}

	if !found {
		return "", 0, fmt.Errorf("could not find host port for container port %d (no matching host port found)", serviceProducerPort)
	}

	hostPort, err := strconv.ParseInt(matchedHostPort.HostPort, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("could not parse host port '%s' as integer", matchedHostPort.HostPort)
	} else if hostPort <= 0 {
		return "", 0, fmt.Errorf("could not find host port for container port %d (invalid host port value %d reported by container orchestrator)", serviceProducerPort, hostPort)
	}

	hostAddress := normalizeHostAddress(matchedHostPort.HostIp)
	log.V(1).Info("Matched service producer port to one of the container host ports", "ServiceProducerPort", serviceProducerPort, "HostPort", hostPort, "HostIP", matchedHostPort.HostIp, "EffectiveHostAddress", hostAddress)
	return hostAddress, int32(hostPort), nil
}

func normalizeHostAddress(hostIP string) string {
	hostAddress := hostIP

	switch hostAddress {
	case "", networking.IPv4AllInterfaceAddress:
		hostAddress = networking.IPv4LocalhostDefaultAddress
	case networking.IPv6AllInterfaceAddress:
		hostAddress = networking.IPv6LocalhostDefaultAddress
	}

	return hostAddress
}
