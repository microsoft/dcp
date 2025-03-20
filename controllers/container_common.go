// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	usvc_slices "github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	volumeInspectionTimeout = 6 * time.Second
)

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
		inspectedCtrs, err := o.InspectContainers(findContext, []string{containerID})
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
		inspectedCtrs, err := o.InspectContainers(findContext, []string{containerID})
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

func inspectManyContainers(ctx context.Context, o containers.ContainerOrchestrator, containerIDs []string) ([]containers.InspectedContainer, error) {
	if len(containerIDs) == 0 {
		return nil, nil
	}

	b := exponentialBackoff(containerInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() ([]containers.InspectedContainer, error) {
		inspectedCtrs, err := o.InspectContainers(ctx, containerIDs)
		if err != nil {
			return nil, err
		}

		if len(inspectedCtrs) == 0 {
			return nil, containers.ErrNotFound
		}

		return inspectedCtrs, nil
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
		_, stopErr := o.StopContainers(ctx, []string{containerID}, stopContainerTimeoutSeconds)
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
		_, removeErr := o.RemoveContainers(ctx, []string{containerID}, true /* force */)
		return removeErr
	}

	verify := func(ctx context.Context) (any, error) {
		// Do not use r.inspectContainer() here, we do not want to retry this when the container is not found
		_, inspectErr := o.InspectContainers(ctx, []string{containerID})

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

func removeManyContainers(removeContext context.Context, o containers.ContainerOrchestrator, containerIDs []string) error {
	if len(containerIDs) == 0 {
		return nil
	}

	action := func(ctx context.Context) error {
		_, err := o.RemoveContainers(ctx, containerIDs, true /* force */)
		return err
	}

	verify := func(ctx context.Context) (any, error) {
		_, inspectErr := o.InspectContainers(ctx, containerIDs)

		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil, nil // This is what we wanted, all containers are gone
		}

		if inspectErr != nil {
			return nil, inspectErr
		} else {
			return nil, fmt.Errorf("some containers still exist after remove")
		}
	}

	_, err := callWithRetryAndVerification(removeContext, defaultContainerOrchestratorBackoff(), action, verify)
	return err
}

func buildImage(buildCtx context.Context, o containers.ContainerOrchestrator, buildOptions containers.BuildImageOptions) (string, error) {
	// Building an image can take a while
	// Since we are using a very long timeout for the operation, we are going to only retry at most once
	buildOptions.Timeout = 10 * time.Minute
	const maxRetries = 1

	action := func(ctx context.Context) error {
		return o.BuildImage(buildCtx, buildOptions)
	}

	verify := func(ctx context.Context) (string, error) {
		iidFile, fileErr := usvc_io.OpenFile(buildOptions.IidFile, os.O_RDONLY, osutil.PermissionOwnerReadWriteOthersRead)
		if fileErr != nil {
			return "", fmt.Errorf("could not open the image ID file: %w", fileErr)
		}
		defer func() {
			_ = iidFile.Close()
			os.Remove(buildOptions.IidFile)
		}()

		reader := bufio.NewReader(iidFile)
		idBytes, _, readErr := reader.ReadLine()
		if readErr != nil {
			return "", fmt.Errorf("could not read the image ID from the ID file: %w", readErr)
		}
		if len(idBytes) == 0 {
			return "", fmt.Errorf("image ID file is empty")
		}
		return string(idBytes), nil
	}

	b := backoff.WithMaxRetries(exponentialBackoff(buildOptions.Timeout), maxRetries)
	imageId, buildErr := callWithRetryAndVerification(buildCtx, b, action, verify)
	return imageId, buildErr
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
	containerObjectName string,
	containerID string,
	streamOptions containers.StreamCommandOptions,
) (*containers.InspectedContainer, error) {
	action := func(ctx context.Context) error {
		_, startErr := o.StartContainers(ctx, []string{containerID}, streamOptions)
		return startErr
	}

	verify := func(ctx context.Context) (*containers.InspectedContainer, error) {
		return verifyContainerState(ctx, o, containerID, func(i *containers.InspectedContainer) error {
			switch i.Status {
			case containers.ContainerStatusRunning:
				return nil

			case containers.ContainerStatusDead:
				return resiliency.Permanent(fmt.Errorf("container '%s' start failed (current state is 'dead')", containerObjectName))

			case containers.ContainerStatusExited:
				// It is possible that the container starts and then exits very quickly afterwards.
				// For the sake of determining whether the startup was successful, we will assume that it was if exit code == 0.
				if i.ExitCode == 0 {
					return nil
				} else {
					return resiliency.Permanent(fmt.Errorf("container '%s' start failed (exit code %d)", containerObjectName, i.ExitCode))
				}

			default:
				errMsg := fmt.Sprintf("status of container '%s' is '%s' (was expecting '%s')", containerObjectName, i.Status, containers.ContainerStatusRunning)
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
			if !slices.ContainsFunc(i.Containers, func(c containers.InspectedNetworkContainer) bool {
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

func connectNetwork(ctx context.Context, o containers.ContainerOrchestrator, opts containers.ConnectNetworkOptions) error {
	action := func(ctx context.Context) error {
		return o.ConnectNetwork(ctx, opts)
	}

	verify := func(ctx context.Context) (*containers.InspectedNetwork, error) {
		return verifyNetworkState(ctx, o, opts.Network, func(i *containers.InspectedNetwork) error {
			if slices.ContainsFunc(i.Containers, func(c containers.InspectedNetworkContainer) bool {
				return c.Name == opts.Container || strings.HasPrefix(c.Id, opts.Container)
			}) {
				return nil
			}
			return fmt.Errorf("container %s is not connected to network %s", opts.Container, opts.Network)
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

func inspectManyNetworks(ctx context.Context, o containers.NetworkOrchestrator, networks []string) ([]containers.InspectedNetwork, error) {
	if len(networks) == 0 {
		return nil, nil
	}

	b := exponentialBackoff(networkInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() ([]containers.InspectedNetwork, error) {
		inspectedNets, err := o.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: networks})
		if err != nil {
			return nil, err
		}

		if len(inspectedNets) == 0 {
			return nil, containers.ErrNotFound
		}

		return inspectedNets, nil
	})
}

func listNetworks(ctx context.Context, o containers.NetworkOrchestrator) ([]containers.ListedNetwork, error) {
	b := exponentialBackoff(networkInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() ([]containers.ListedNetwork, error) {
		return o.ListNetworks(ctx)
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

func removeManyNetworks(ctx context.Context, o containers.NetworkOrchestrator, networkIDs []string, log logr.Logger) error {
	if len(networkIDs) == 0 {
		return nil
	}

	action := func(ctx context.Context) error {
		_, err := o.RemoveNetworks(ctx, containers.RemoveNetworksOptions{Networks: networkIDs, Force: true})
		if err != nil {
			// Network removal has been particularly problematic in the past, so we want extra logging.
			log.V(1).Info("Some container networks could not be removed", "networks", networkIDs, "error", err.Error())
		}
		return err
	}

	verify := func(ctx context.Context) (any, error) {
		networks, listErr := o.ListNetworks(ctx)

		if listErr != nil {
			return nil, listErr
		}

		existingNetworkIds := usvc_slices.Map[containers.ListedNetwork, string](networks, func(n containers.ListedNetwork) string {
			return n.ID
		})

		deleted, _ := usvc_slices.Diff(networkIDs, existingNetworkIds)
		if len(deleted) == len(networkIDs) {
			return nil, nil
		} else {
			remaining, _ := usvc_slices.Diff(networkIDs, deleted)
			return nil, fmt.Errorf("some networks still exist: %v", remaining)
		}
	}

	_, err := callWithRetryAndVerification(ctx, defaultContainerOrchestratorBackoff(), action, verify)
	return err
}

func inspectContainerVolume(ctx context.Context, o containers.VolumeOrchestrator, volumeName string) (*containers.InspectedVolume, error) {
	b := exponentialBackoff(volumeInspectionTimeout)
	return resiliency.RetryGet(ctx, b, func() (*containers.InspectedVolume, error) {
		inspectedVolumes, err := o.InspectVolumes(ctx, []string{volumeName})
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
		inspectedVolumes, err := o.InspectVolumes(ctx, []string{volumeName})
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
		_, err := o.RemoveVolumes(ctx, []string{volumeName}, false /* force */)
		if errors.Is(err, containers.ErrObjectInUse) {
			// Treat this error as permanent and let the caller decide how to handle it
			// (e.g. wait, or retry, or try to remove containers that use the volume).
			return backoff.Permanent(err)
		}
		return err
	}

	verify := func(ctx context.Context) (any, error) {
		// Do not use r.inspectContainerVolume() here, we do not want to retry this when the volume is not found
		_, inspectErr := o.InspectVolumes(ctx, []string{volumeName})

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
		err := o.CreateVolume(ctx, volumeName)

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
