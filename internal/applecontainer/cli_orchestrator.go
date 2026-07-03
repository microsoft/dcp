/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package applecontainer implements a container orchestrator backed by the Apple `container` CLI
// (https://github.com/apple/container), available on macOS (Apple Silicon) only.
//
// The Apple container runtime differs from Docker/Podman in several important ways that shape
// this implementation:
//
//   - Containers can only be attached to networks at creation time; there is no
//     `network connect` / `network disconnect`. DCP maps all DCP-managed networks onto the
//     built-in "default" network: CreateNetwork returns "default", ConnectNetwork verifies the
//     container is attached, and DisconnectNetwork is a no-op. Containers on the default network
//     can reach each other directly via their routable IPs.
//   - There is no network alias support and the runtime's DNS serves no records for container
//     names. DCP runs a small DNS forwarder in a container and points every container at it,
//     so container names and network aliases resolve (see dns_forwarder.go).
//   - There is no `host.docker.internal` equivalent. The host is reachable from inside a
//     container via the network gateway IP, which ContainerHost() reports.
//   - There is no `events` command; container and network events are synthesized by polling.
//   - `inspect` commands are all-or-nothing, so multi-object inspections are issued one
//     object at a time to allow partial success.
//   - Exit codes of detached containers are not reported by the runtime, so InspectContainers
//     always reports exit code 0. (`container start --attach` does propagate the exit code, which
//     a future improvement could use by keeping an attached observer process per container.)
//   - Files can only be copied into a *running* container (see
//     CreateFilesRequiresRunningContainer), so files that must be present before a container
//     starts are baked into a derived image by the container controller instead.
//   - `--label` values cannot contain '='; such values are encoded on the way in and decoded
//     when read back (see labels.go).
package applecontainer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/internal/termpty"
)

const (
	// Name of the built-in network every container is attached to by default.
	defaultNetworkName = "default"

	// Used as the container host name if the default network gateway cannot be determined.
	// 192.168.65.1 is the documented default gateway for the built-in "default" network.
	fallbackContainerHost = "192.168.65.1"
)

var (
	containerNotFoundRegEx   = regexp.MustCompile(`(?i)container (not found|with ID (.*) not found)`)
	volumeNotFoundRegEx      = regexp.MustCompile(`(?i)volume not found`)
	volumeAlreadyExistsRegEx = regexp.MustCompile(`(?i)volume '(.*)' already exists`)
	volumeDeleteFailedRegEx  = regexp.MustCompile(`(?i)failed to delete one or more volumes`)
	networkNotFoundRegEx     = regexp.MustCompile(`(?i)network not found`)
	networkDeleteFailedRegEx = regexp.MustCompile(`(?i)failed to delete one or more networks`)
	imageNotFoundRegEx       = regexp.MustCompile(`(?i)image not found`)
	runtimeNotRunningRegEx   = regexp.MustCompile(`(?i)(XPC connection error|connection refused|failed to connect|services (are )?not running|container system start)`)

	newContainerNotFoundErrorMatch = containers.NewCliErrorMatch(containerNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
	newVolumeNotFoundErrorMatch    = containers.NewCliErrorMatch(volumeNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
	volumeAlreadyExistsErrorMatch  = containers.NewCliErrorMatch(volumeAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("volume already exists")))
	volumeDeleteFailedErrorMatch   = containers.NewCliErrorMatch(volumeDeleteFailedRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume could not be deleted")))
	newNetworkNotFoundErrorMatch   = containers.NewCliErrorMatch(networkNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
	networkDeleteFailedErrorMatch  = containers.NewCliErrorMatch(networkDeleteFailedRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network could not be deleted")))
	imageNotFoundErrorMatch        = containers.NewCliErrorMatch(imageNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("image not found")))
	runtimeNotRunningErrorMatch    = containers.NewCliErrorMatch(runtimeNotRunningRegEx, errors.Join(containers.ErrRuntimeNotHealthy, fmt.Errorf("the Apple container runtime is not healthy")))

	// We expect almost all `container` CLI invocations to finish within this time.
	ordinaryCommandTimeout = 30 * time.Second

	// We allow up to a minute for diagnostic commands to finish as we'd rather wait a bit longer than miss information.
	diagnosticCommandTimeout = 1 * time.Minute

	defaultBuildImageTimeout      = 10 * time.Minute
	defaultPullImageTimeout       = 10 * time.Minute
	defaultCreateContainerTimeout = 10 * time.Minute
	defaultRunContainerTimeout    = 10 * time.Minute

	// Cache and synchronization control for checking runtime cachedStatus
	cachedStatus *containers.ContainerRuntimeStatus
	// Ensure that only one goroutine is checking the status at a time
	checkStatusLock = concurrency.NewContextAwareLock()
	// Mutex to control read/write access to the cached status
	updateStatus            = &sync.RWMutex{}
	backgroundStatusUpdates atomic.Int32

	// Cached container host (default network gateway IP)
	containerHostLock   = &sync.Mutex{}
	cachedContainerHost string
)

type AppleContainerCliOrchestrator struct {
	log logr.Logger

	// Process executor for running `container` commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]

	// Event watcher for network events
	networkEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]

	// State of the DNS forwarder that provides network alias resolution (see dns_forwarder.go)
	dns dnsForwarderState
}

func NewAppleContainerCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	aco := &AppleContainerCliOrchestrator{
		log:      log,
		executor: executor,
	}

	aco.containerEvtWatcher = pubsub.NewSubscriptionSet(aco.doWatchContainers, context.Background())
	aco.networkEvtWatcher = pubsub.NewSubscriptionSet(aco.doWatchNetworks, context.Background())

	return aco
}

func (*AppleContainerCliOrchestrator) IsDefault() bool {
	return false
}

func (*AppleContainerCliOrchestrator) Name() string {
	return "container"
}

// ContainerHost returns the address at which the host machine is reachable from inside a container.
// The Apple container runtime has no built-in DNS name for the host (like host.docker.internal);
// instead the host is reachable via the gateway IP of the container network, so we report
// the gateway of the built-in default network.
func (aco *AppleContainerCliOrchestrator) ContainerHost() string {
	containerHostLock.Lock()
	defer containerHostLock.Unlock()

	if cachedContainerHost != "" {
		return cachedContainerHost
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cachedContainerHost = fallbackContainerHost
	if network, err := aco.inspectSingleNetwork(ctx, defaultNetworkName); err == nil && len(network.Gateways) > 0 && network.Gateways[0] != "" {
		cachedContainerHost = network.Gateways[0]
	}

	return cachedContainerHost
}

func (aco *AppleContainerCliOrchestrator) CheckStatus(ctx context.Context, cacheUsage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	// A cached status is already available, return it
	updateStatus.RLock()
	if cachedStatus != nil && cacheUsage == containers.CachedRuntimeStatusAllowed {
		defer updateStatus.RUnlock()
		return *cachedStatus
	}
	updateStatus.RUnlock()

	if cacheUsage == containers.CachedRuntimeStatusAllowed {
		// For cached results, only one goroutine should be checking the status at a time
		if syncErr := checkStatusLock.Lock(ctx); syncErr != nil {
			// Timed out, assume the runtime is not responsive and unavailable
			return containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     "Timed out while checking Apple container runtime status; the `container` CLI is not responsive.",
			}
		}

		defer checkStatusLock.Unlock()
	}

	updateStatus.RLock()
	// Check again if the status is available in the cache
	if cachedStatus != nil && cacheUsage == containers.CachedRuntimeStatusAllowed {
		defer updateStatus.RUnlock()
		return *cachedStatus
	}
	updateStatus.RUnlock()

	newStatus := aco.getStatus(ctx)

	updateStatus.Lock()
	// Update the cached status
	cachedStatus = &newStatus
	updateStatus.Unlock()

	return newStatus
}

// Check the status of the Apple container runtime in the background until the context is canceled.
func (aco *AppleContainerCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if !backgroundStatusUpdates.CompareAndSwap(0, 1) {
		return
	}

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		for {
			// Only one goroutine should be checking the status at a time
			if checkStatusLock.TryLock() {
				newStatus := aco.getStatus(ctx)

				updateStatus.Lock()
				// Update the cached status
				cachedStatus = &newStatus
				updateStatus.Unlock()

				checkStatusLock.Unlock()
			}

			// Wait for 5 seconds before checking again
			timer.Reset(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				continue
			}
		}
	}()
}

func (aco *AppleContainerCliOrchestrator) getStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	// The Apple container runtime is only available on macOS (Apple Silicon).
	if goruntime.GOOS != "darwin" {
		return containers.ContainerRuntimeStatus{
			Installed: false,
			Running:   false,
			Error:     "The Apple container runtime is only supported on macOS.",
		}
	}

	// The Apple container CLI always includes the built-in default network and formats its response
	// as a JSON array, so we can use this to ensure we're talking to the Apple container CLI and not
	// some other binary that happens to be named "container".
	_, err := aco.ListNetworks(ctx, containers.ListNetworksOptions{})

	var invalidUnmarshalError *json.InvalidUnmarshalError
	var unmarshalTypeError *json.UnmarshalTypeError
	var syntaxError *json.SyntaxError

	if errors.Is(err, exec.ErrNotFound) {
		// Try to get the inner error if this is an exec.ErrNotFound error
		// The outer error is potentially a "DCP" error and not the actual error from exec
		// which makes it harder for users to understand that the error is simply that we
		// couldn't find the `container` binary on their path.
		if unwrapErr := errors.Unwrap(err); errors.Is(unwrapErr, exec.ErrNotFound) {
			err = unwrapErr
		}

		// Couldn't find the `container` CLI, so it's not installed
		return containers.ContainerRuntimeStatus{
			Installed: false,
			Running:   false,
			Error:     err.Error(),
		}
	} else if errors.Is(err, context.DeadlineExceeded) {
		// Timed out, assume the runtime is not responsive but available
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Running:   false,
			Error:     "The `container` CLI timed out while checking status. Ensure the Apple container CLI is functioning correctly and try again.",
		}
	} else if errors.As(err, &invalidUnmarshalError) || errors.As(err, &unmarshalTypeError) || errors.As(err, &syntaxError) {
		return containers.ContainerRuntimeStatus{
			Installed: false,
			Running:   false,
			Error:     "Output from the `container` CLI didn't match the expected format. The `container` CLI found on your PATH may not be a valid Apple container installation.",
		}
	} else if errors.Is(err, containers.ErrRuntimeNotHealthy) {
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Running:   false,
			Error:     "The Apple container system services are not running. Start them with 'container system start'.",
		}
	} else if err != nil {
		// Error response from the `container` command, assume runtime isn't available
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Running:   false,
			Error:     err.Error(),
		}
	}

	// The command returned successfully, assume runtime is ready
	return containers.ContainerRuntimeStatus{
		Installed: true,
		Running:   true,
	}
}

func (aco *AppleContainerCliOrchestrator) GetDiagnostics(ctx context.Context) (containers.ContainerDiagnostics, error) {
	cmd := makeAppleContainerCommand("system", "status", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "Version", cmd, nil, nil, diagnosticCommandTimeout)
	if err != nil {
		return containers.ContainerDiagnostics{}, errors.Join(err, normalizeCliErrors(errBuf))
	}

	var diagnostics containers.ContainerDiagnostics
	if unmarshalErr := unmarshalDiagnostics(outBuf.Bytes(), &diagnostics); unmarshalErr != nil {
		return containers.ContainerDiagnostics{}, unmarshalErr
	}

	versionCmd := makeAppleContainerCommand("--version")
	versionOut, _, versionErr := aco.runBufferedCommand(ctx, "ClientVersion", versionCmd, nil, nil, diagnosticCommandTimeout)
	if versionErr == nil {
		diagnostics.ClientVersion = extractVersion(versionOut.String())
	}

	return diagnostics, nil
}

// VOLUME MANAGEMENT

func (aco *AppleContainerCliOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	cmd := makeAppleContainerCommand("volume", "create", options.Name)
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "CreateVolume", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		// Note: unlike Docker, the Apple container CLI returns an error if the volume already exists.
		return errors.Join(err, normalizeCliErrors(errBuf, volumeAlreadyExistsErrorMatch))
	}

	return containers.ExpectCliStrings(outBuf, []string{options.Name})
}

// InspectVolumes returns the details of successfully inspected volumes, and a slice of all errors encountered during the inspection.
// The Apple container CLI fails the whole inspection if any of the requested volumes is missing,
// so volumes are inspected one at a time to allow partial success.
func (aco *AppleContainerCliOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	var inspectedVolumes []containers.InspectedVolume
	var err error

	for _, volume := range options.Volumes {
		cmd := makeAppleContainerCommand("volume", "inspect", volume)
		outBuf, errBuf, cmdErr := aco.runBufferedCommand(ctx, "InspectVolumes", cmd, nil, nil, ordinaryCommandTimeout)
		if cmdErr != nil {
			err = errors.Join(err, cmdErr, normalizeCliErrors(errBuf, newVolumeNotFoundErrorMatch))
			continue
		}

		volumes, unmarshalErr := asArrayOfObjects(outBuf, unmarshalVolume)
		err = errors.Join(err, unmarshalErr)
		inspectedVolumes = append(inspectedVolumes, volumes...)
	}

	if len(inspectedVolumes) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v volumes were successfully inspected", len(inspectedVolumes), len(options.Volumes))))
	}

	return inspectedVolumes, err
}

func (aco *AppleContainerCliOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	// Note: the Apple container CLI does not support force-removal of volumes; the Force option is ignored.
	args := []string{"volume", "rm"}
	args = append(args, options.Volumes...)

	cmd := makeAppleContainerCommand(args...)

	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "RemoveVolumes", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, volumeDeleteFailedErrorMatch.MaxObjects(len(options.Volumes))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(removed) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v volumes were successfully removed", len(removed), len(options.Volumes))))
	}

	return removed, err
}

// IMAGE MANAGEMENT

func (aco *AppleContainerCliOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	args := []string{"build"}

	if options.Dockerfile != "" {
		args = append(args, "-f", options.Dockerfile)
	}

	// Should base images be updated even if they are already present locally?
	if options.Pull {
		args = append(args, "--pull")
	}

	// The Apple container CLI does not support --iidfile; if an image ID file is requested,
	// the image is resolved by tag after the build finishes (see below). Make sure there is
	// at least one tag to resolve the image by.
	tags := options.Tags
	if len(tags) == 0 && options.IidFile != "" {
		tags = []string{fmt.Sprintf("dcp-build-%d", time.Now().UnixNano())}
	}

	// The Apple container CLI accepts a single tag; additional tags are applied after the build.
	if len(tags) > 0 {
		args = append(args, "-t", tags[0])
	}

	// Apply all specified build arguments
	for _, buildArg := range options.Args {
		if buildArg.Value != "" {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", buildArg.Name, buildArg.Value))
		} else {
			args = append(args, "--build-arg", buildArg.Name)
		}
	}

	// Secret values that need to be applied to the build command environment
	secretEnvironment := map[string]string{}

	// Apply all specified build secrets
	for _, secret := range options.Secrets {
		switch secret.Type {
		case apiv1.FileSecret, "":
			args = append(args, "--secret", fmt.Sprintf("id=%s,src=%s", secret.ID, secret.Source))
		case apiv1.EnvSecret:
			if secret.Source != "" {
				args = append(args, "--secret", fmt.Sprintf("id=%s,env=%s", secret.ID, secret.Source))
				if secret.Value != "" {
					secretEnvironment[secret.Source] = secret.Value
				}
			} else {
				args = append(args, "--secret", fmt.Sprintf("id=%s,env=%s", secret.ID, secret.ID))
				if secret.Value != "" {
					secretEnvironment[secret.ID] = secret.Value
				}
			}
		}
	}

	// If a build stage is given, use it
	if options.Stage != "" {
		args = append(args, "--target", options.Stage)
	}

	// Apply any specified labels
	for _, label := range options.Labels {
		args = append(args, "--label", labelArg(label.Key, label.Value))
	}

	// If a target platform is specified, build for that platform
	if options.Platform != "" {
		args = append(args, "--platform", options.Platform)
	}

	// Enable plain output mode
	args = append(args, "--progress", "plain")

	// Append the build context argument
	args = append(args, options.Context)

	cmd := makeAppleContainerCommand(args...)

	// Append secret environment
	cmd.Env = os.Environ()
	for secretName, secretValue := range secretEnvironment {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", secretName, secretValue))
	}

	// Building an image can take a long time to finish, particularly if any base images are not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultBuildImageTimeout
	}
	_, errBuf, err := aco.runBufferedCommand(ctx, "BuildImage", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return errors.Join(err, normalizeCliErrors(errBuf))
	}

	// Apply any additional tags
	for _, extraTag := range tags[min(1, len(tags)):] {
		tagCmd := makeAppleContainerCommand("image", "tag", tags[0], extraTag)
		if _, tagErrBuf, tagErr := aco.runBufferedCommand(ctx, "TagImage", tagCmd, nil, nil, ordinaryCommandTimeout); tagErr != nil {
			return errors.Join(tagErr, normalizeCliErrors(tagErrBuf))
		}
	}

	// The Apple container CLI does not support --iidfile, so resolve the image ID by tag and write the file ourselves.
	if options.IidFile != "" {
		inspected, inspectErr := aco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{tags[0]}})
		if inspectErr != nil || len(inspected) == 0 {
			return errors.Join(inspectErr, fmt.Errorf("could not resolve the ID of the built image '%s'", tags[0]))
		}
		if writeErr := os.WriteFile(options.IidFile, []byte(inspected[0].Id), 0644); writeErr != nil {
			return fmt.Errorf("could not write image ID file '%s': %w", options.IidFile, writeErr)
		}
	}

	return nil
}

// InspectImages inspects images one at a time (the Apple container CLI fails the whole
// inspection if any of the requested images is missing).
func (aco *AppleContainerCliOrchestrator) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if len(options.Images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}

	var inspectedImages []containers.InspectedImage
	var err error

	for _, image := range options.Images {
		cmd := makeAppleContainerCommand("image", "inspect", image)
		outBuf, errBuf, cmdErr := aco.runBufferedCommand(ctx, "InspectImages", cmd, nil, nil, ordinaryCommandTimeout)
		if cmdErr != nil {
			err = errors.Join(err, cmdErr, normalizeCliErrors(errBuf, imageNotFoundErrorMatch))
			continue
		}

		images, unmarshalErr := asArrayOfObjects(outBuf, unmarshalImage)
		err = errors.Join(err, unmarshalErr)
		inspectedImages = append(inspectedImages, images...)
	}

	if len(inspectedImages) < len(options.Images) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v images were successfully inspected", len(inspectedImages), len(options.Images))))
	}

	return inspectedImages, err
}

func (aco *AppleContainerCliOrchestrator) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	if options.Image == "" {
		return "", fmt.Errorf("must specify an image to pull")
	}

	imageRef := options.Image
	if options.Digest != "" {
		imageRef = options.Image + "@" + options.Digest
	}

	cmd := makeAppleContainerCommand("image", "pull", "--progress", "none", imageRef)
	// Pulling large images can take a long time, especially if the image is not available locally and the network is slow.
	if options.Timeout == 0 {
		options.Timeout = defaultPullImageTimeout
	}
	_, errBuf, err := aco.runBufferedCommand(ctx, "PullImage", cmd, nil, nil, options.Timeout)
	if err != nil {
		return "", errors.Join(err, normalizeCliErrors(errBuf, imageNotFoundErrorMatch))
	}

	// The pull command does not print the image ID, so resolve it via image inspect.
	inspected, inspectErr := aco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{imageRef}})
	if inspectErr != nil || len(inspected) == 0 {
		// Best effort: the pull itself succeeded, so return the reference if the image ID
		// cannot be resolved.
		return imageRef, nil
	}

	return inspected[0].Id, nil
}

// ApplyImageLayersImpl in the containers package streams the build context to the runtime via stdin,
// which the Apple container CLI does not support. Instead, the build context (Dockerfile + layer tars)
// is materialized in a temporary directory and built from there.
func (aco *AppleContainerCliOrchestrator) ApplyImageLayers(ctx context.Context, options containers.ApplyImageLayersOptions) (string, error) {
	if len(options.Layers) == 0 {
		return "", fmt.Errorf("at least one image layer must be specified")
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultBuildImageTimeout
	}

	// Use a tag if available, otherwise fall back to the image ID for the FROM directive
	baseImage := options.BaseImage.Id
	if len(options.BaseImage.Tags) > 0 {
		baseImage = options.BaseImage.Tags[0]
	}

	contextDir, mkdirErr := os.MkdirTemp("", "dcp-image-layers-")
	if mkdirErr != nil {
		return "", fmt.Errorf("creating temporary build context directory: %w", mkdirErr)
	}
	defer func() { _ = os.RemoveAll(contextDir) }()

	dockerfile := fmt.Sprintf("FROM %s\n", baseImage)
	for i := range options.Layers {
		layer := &options.Layers[i]
		layerFileName := fmt.Sprintf("layer%d.tar", i)

		if writeErr := writeLayerToFile(layer, contextDir, layerFileName); writeErr != nil {
			return "", fmt.Errorf("materializing image layer %d: %w", i, writeErr)
		}

		dockerfile += fmt.Sprintf("ADD %s /\n", layerFileName)
	}

	if writeErr := os.WriteFile(contextDir+string(os.PathSeparator)+"Dockerfile", []byte(dockerfile), 0644); writeErr != nil {
		return "", fmt.Errorf("writing Dockerfile to build context: %w", writeErr)
	}

	tag := options.Tag
	if tag == "" {
		tag = fmt.Sprintf("dcp-layered-%d", time.Now().UnixNano())
	}

	cmd := makeAppleContainerCommand("build", "--progress", "plain", "-t", tag, contextDir)

	_, errBuf, buildErr := aco.runBufferedCommand(ctx, "ApplyImageLayers", cmd, nil, nil, timeout)
	if buildErr != nil {
		return "", fmt.Errorf("building derived image with image layers: %w: %s", buildErr, errBuf.String())
	}

	aco.log.V(1).Info("Built derived image with image layers", "ImageRef", tag, "LayerCount", len(options.Layers))

	return tag, nil
}

// CONTAINER MANAGEMENT

func (aco *AppleContainerCliOrchestrator) applyCreateContainerOptions(args []string, options containers.CreateContainerOptions) []string {
	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	// The Apple container runtime supports repeating --network to attach multiple networks at
	// creation time, but has no --network-alias equivalent; aliases are resolved by the DCP DNS
	// forwarder instead (see dns_forwarder.go). Networks are deduplicated because DCP-managed
	// networks all map onto the built-in default network (see UsesSingleNetwork), so multiple
	// requested networks typically resolve to the same runtime network.
	attachedNetworks := map[string]bool{}
	for _, network := range options.Networks {
		if network.Name == "" || attachedNetworks[network.Name] {
			continue
		}
		attachedNetworks[network.Name] = true
		args = append(args, "--network", network.Name)
	}

	for _, mount := range options.VolumeMounts {
		mountVal := fmt.Sprintf("type=%s", mount.Type)
		if mount.Source != "" {
			mountVal += fmt.Sprintf(",source=%s", mount.Source)
		}
		mountVal += fmt.Sprintf(",target=%s", mount.Target)
		if mount.ReadOnly {
			mountVal += ",readonly"
		}
		args = append(args, "--mount", mountVal)
	}

	for _, port := range options.Ports {
		hostIP := port.HostIP
		if hostIP == "" {
			// Bind to 127.0.0.1 for extra security, not to 0.0.0.0 (all interfaces, making it accessible from the outside)
			// IPv6 is not well supported with container networking, so we assume IPv4. We'll need to revisit this logic
			// if we start getting requests to support IPv6 container networking.
			hostIP = networking.IPv4LocalhostDefaultAddress
		}

		hostPort := port.HostPort
		if hostPort == 0 {
			// The Apple container CLI requires an explicit host port in the publish specification,
			// so allocate a free one ourselves.
			protocol := port.Protocol
			if protocol == "" {
				protocol = apiv1.TCP
			}
			freePort, portErr := networking.GetFreePort(protocol, hostIP, aco.log)
			if portErr != nil {
				// Let the runtime report the invalid (zero) port; there is no good way to recover here.
				aco.log.Error(portErr, "Could not allocate a free host port for container port", "ContainerPort", port.ContainerPort)
			}
			hostPort = freePort
		}

		portVal := fmt.Sprintf("%s:%d:%d", hostIP, hostPort, port.ContainerPort)

		if port.Protocol != "" {
			portVal = fmt.Sprintf("%s/%s", portVal, strings.ToLower(string(port.Protocol)))
		}

		args = append(args, "-p", portVal)
	}

	for _, envVar := range options.Env {
		eVal := fmt.Sprintf("%s=%s", envVar.Name, envVar.Value)
		args = append(args, "-e", eVal)
	}

	for _, envFile := range options.EnvFiles {
		args = append(args, "--env-file", envFile)
	}

	for _, label := range options.Labels {
		args = append(args, "--label", labelArg(label.Key, label.Value))
	}

	if options.RestartPolicy != "" && options.RestartPolicy != apiv1.RestartPolicyNone {
		// The Apple container runtime has no restart policy support.
		aco.log.V(1).Info("Restart policies are not supported by the Apple container runtime and will be ignored", "RestartPolicy", options.RestartPolicy)
	}

	// Note: the Apple container runtime has no pull policy flag; images are pulled if (and only if)
	// they are not available locally, which matches the "missing" pull policy.
	if options.PullPolicy == apiv1.PullPolicyAlways {
		aco.log.V(1).Info("Pull policy 'always' is not supported by the Apple container runtime; the image will only be pulled if not available locally")
	}

	if options.Command != "" {
		args = append(args, "--entrypoint", options.Command)
	}

	if len(options.Healthcheck.Command) > 0 {
		// The Apple container runtime has no health check support.
		aco.log.V(1).Info("Container health checks are not supported by the Apple container runtime and will be ignored")
	}

	if options.Terminal != nil {
		// Attach a TTY (-t) and keep STDIN open (-i) if a terminal is requested
		args = append(args, "-it")
	}

	args = append(args, options.RunArgs...)

	return args
}

func (aco *AppleContainerCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	args := []string{"create"}

	args = aco.applyCreateContainerOptions(args, options)

	// Point the container at the DCP DNS forwarder so container names and network aliases
	// resolve (see dns_forwarder.go). If the forwarder is unavailable the container keeps
	// the runtime default (gateway) resolver and only alias resolution is degraded.
	if dnsIP := aco.ensureDnsForwarder(ctx); dnsIP != "" {
		args = append(args, "--dns", dnsIP)
	}
	aco.registerPendingDnsNames(options.Name, options.Networks)

	args = append(args, options.Image)

	if len(options.Args) > 0 {
		args = append(args, options.Args...)
	}

	cmd := makeAppleContainerCommand(args...)

	// Create container can take a long time to finish if the image is not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultCreateContainerTimeout
	}

	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "CreateContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf))
		if id, err2 := asId(outBuf); err2 == nil {
			// We got an ID, so the container was created, but the command failed.
			return id, err
		}

		return "", err
	}

	return asId(outBuf)
}

func (aco *AppleContainerCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	args := []string{"run"}

	args = aco.applyCreateContainerOptions(args, options.CreateContainerOptions)

	// See CreateContainer for the DNS forwarder handling.
	if dnsIP := aco.ensureDnsForwarder(ctx); dnsIP != "" {
		args = append(args, "--dns", dnsIP)
	}
	aco.registerPendingDnsNames(options.Name, options.Networks)

	args = append(args, "--detach")
	args = append(args, options.Image)

	if len(options.Args) > 0 {
		args = append(args, options.Args...)
	}

	cmd := makeAppleContainerCommand(args...)

	// The run container command can take a long time to finish if the image is not available locally.
	// So we use much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultRunContainerTimeout
	}

	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "RunContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return "", errors.Join(err, normalizeCliErrors(errBuf))
	}

	id, idErr := asId(outBuf)
	if idErr == nil {
		aco.publishDnsRecords(ctx, id)
	}
	return id, idErr
}

func (aco *AppleContainerCliOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	args := []string{"exec"}

	if options.WorkingDirectory != "" {
		args = append(args, "--workdir", options.WorkingDirectory)
	}

	for _, envVar := range options.Env {
		eVal := fmt.Sprintf("%s=%s", envVar.Name, envVar.Value)
		args = append(args, "-e", eVal)
	}

	for _, envFile := range options.EnvFiles {
		args = append(args, "--env-file", envFile)
	}

	args = append(args, options.Container)
	args = append(args, options.Command)
	if len(options.Args) > 0 {
		args = append(args, options.Args...)
	}

	cmd := makeAppleContainerCommand(args...)

	cmd.Stdout = options.StdOutStream
	cmd.Stderr = options.StdErrStream

	exitCh := make(chan int32)
	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		// We only care about the exit code, not the error. The only scenario where we should get an error
		// is if the context for an exec command is canceled during DCP shutdown, in which case that's expected.
		if err != nil && !errors.Is(err, context.Canceled) {
			aco.log.Error(err, "Unexpected error during container exec command", "Command", cmd.String())
		}
		exitCh <- exitCode
		close(exitCh)
	}

	aco.log.V(1).Info("Running Apple container command", "Command", cmd.String())
	_, startWaitForProcessExit, err := aco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler), process.CreationFlagsNone, nil)
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start Apple container command '%s'", "ExecContainer"))
	}
	startWaitForProcessExit()

	return exitCh, nil
}

// AttachContainer is not supported: the Apple container CLI has no `attach` command.
func (aco *AppleContainerCliOrchestrator) AttachContainer(ctx context.Context, options containers.AttachContainerOptions) (*termpty.PseudoTerminalProcess, error) {
	return nil, fmt.Errorf("attaching a terminal to a container is not supported by the Apple container runtime")
}

func (aco *AppleContainerCliOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	// Note: the Apple container CLI has no server-side label filtering; filters are applied client-side.
	cmd := makeAppleContainerCommand("ls", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "ListContainers", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		// If the command failed, return the error
		return nil, errors.Join(err, normalizeCliErrors(errBuf))
	}

	listed, unmarshalErr := asArrayOfObjects(outBuf, unmarshalListedContainer)
	if unmarshalErr != nil {
		return nil, unmarshalErr
	}

	return filterByLabels(listed, options.Filters.LabelFilters, func(lc containers.ListedContainer) map[string]string { return lc.Labels }), nil
}

// InspectContainers inspects containers one at a time (the Apple container CLI fails the whole
// inspection if any of the requested containers is missing).
func (aco *AppleContainerCliOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var inspectedContainers []containers.InspectedContainer
	var err error

	for _, container := range options.Containers {
		cmd := makeAppleContainerCommand("inspect", container)
		outBuf, errBuf, cmdErr := aco.runBufferedCommand(ctx, "InspectContainers", cmd, nil, nil, ordinaryCommandTimeout)
		if cmdErr != nil {
			err = errors.Join(err, cmdErr, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch))
			continue
		}

		inspected, unmarshalErr := asArrayOfObjects(outBuf, unmarshalContainer)
		err = errors.Join(err, unmarshalErr)
		inspectedContainers = append(inspectedContainers, inspected...)
	}

	if len(inspectedContainers) < len(options.Containers) {
		// We're returning an incomplete set of inspected containers
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v containers were successfully inspected", len(inspectedContainers), len(options.Containers))))
	}

	return inspectedContainers, err
}

// StartContainers starts containers one at a time (the Apple container CLI `start` command
// accepts a single container only).
func (aco *AppleContainerCliOrchestrator) StartContainers(ctx context.Context, options containers.StartContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var started []string
	var err error

	for _, container := range options.Containers {
		cmd := makeAppleContainerCommand("start", container)
		outBuf, errBuf, cmdErr := aco.runBufferedCommand(ctx, "StartContainers", cmd, options.StdOutStream, options.StdErrStream, ordinaryCommandTimeout)
		if cmdErr != nil {
			err = errors.Join(err, cmdErr, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch))
			continue
		}

		if id, idErr := asId(outBuf); idErr == nil {
			started = append(started, id)
		} else {
			started = append(started, container)
		}

		// The container now has an IP address; publish its DNS names to the forwarder.
		aco.publishDnsRecords(ctx, container)
	}

	if len(started) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v containers were successfully started", len(started), len(options.Containers))))
	}

	return started, err
}

func (aco *AppleContainerCliOrchestrator) StopContainers(ctx context.Context, options containers.StopContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"stop"}
	var timeout time.Duration = ordinaryCommandTimeout
	if options.SecondsToKill > 0 {
		args = append(args, "--time", fmt.Sprintf("%d", options.SecondsToKill))
		timeout = time.Duration(options.SecondsToKill)*time.Second + ordinaryCommandTimeout
	}
	args = append(args, options.Containers...)

	cmd := makeAppleContainerCommand(args...)
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "StopContainers", cmd, nil, nil, timeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	stopped := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	// The stopped containers' IP addresses are no longer valid; retract their DNS records.
	aco.unpublishDnsRecords(stopped)

	if len(stopped) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v containers were successfully stopped", len(stopped), len(options.Containers))))
	}

	return stopped, err
}

func (aco *AppleContainerCliOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"rm"}
	if options.Force {
		args = append(args, "--force")
	}
	args = append(args, options.Containers...)

	cmd := makeAppleContainerCommand(args...)
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "RemoveContainers", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	aco.unpublishDnsRecords(removed)

	if len(removed) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v containers were successfully removed", len(removed), len(options.Containers))))
	}

	return removed, err
}

// CreateFilesRequiresRunningContainer reports that the Apple container runtime can only inject
// files into a running container. The container controller responds by baking configured files
// and certificates into a derived image at creation time (so they are present before the
// entrypoint runs) instead of calling CreateFiles on a created-but-not-started container.
func (aco *AppleContainerCliOrchestrator) CreateFilesRequiresRunningContainer() bool {
	return true
}

// CreateFiles copies files into a container by streaming a tar archive through
// `container exec ... tar -x`. The Apple container CLI can only copy files into a
// *running* container (and `container cp` does not preserve file ownership), so this
// requires the container to be running and the container image to include a `tar` binary.
// Files that must be present before the container starts are baked into a derived image
// by the container controller instead (see CreateFilesRequiresRunningContainer).
func (aco *AppleContainerCliOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	tarBytes, tarErr := containers.BuildCreateFilesTar(options, aco.log)
	if tarErr != nil {
		return tarErr
	}
	if tarBytes == nil {
		// Can happen if all ContinueOnError items fail
		return nil
	}

	cmd := makeAppleContainerCommand("exec", "-i", options.Container, "tar", "-xpf", "-", "-C", "/")
	cmd.Stdin = bytes.NewReader(tarBytes)
	_, errBuf, err := aco.runBufferedCommand(ctx, "CreateFiles", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return errors.Join(
			err,
			normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch),
			fmt.Errorf("the Apple container runtime can only copy files into a running container whose image provides a 'tar' binary"))
	}

	return nil
}

func (aco *AppleContainerCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := aco.containerEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (aco *AppleContainerCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout usvc_io.WriteSyncerCloser, stderr usvc_io.WriteSyncerCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"logs"}
	if options.Follow {
		args = append(args, "--follow")
	}
	args = append(args, container)

	cmd := makeAppleContainerCommand(args...)

	// The Apple container CLI has no --timestamps option; if timestamps are requested,
	// stamp each line with the time it was received, mimicking the Docker/Podman output format.
	var stdOutWriter io.Writer = stdout
	var stdErrWriter io.Writer = stderr
	if options.Timestamps {
		stdOutWriter = newTimestampingWriter(stdout)
		stdErrWriter = newTimestampingWriter(stderr)
	}

	exitCh, err := aco.streamCommand(ctx, "CaptureContainerLogs", cmd, stdOutWriter, stdErrWriter, streamCommandOptionUseWatcher)
	if err != nil {
		return err
	}

	go func() {
		// Wait for the command to finish and clean up any resources
		exitErr := <-exitCh
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) && !errors.Is(exitErr, context.DeadlineExceeded) {
			aco.log.Error(exitErr, "Capturing container logs failed", "Container", container)
		}

		if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
			aco.log.Error(stdOutCloseErr, "Closing stdout log destination failed", "Container", container)
		}
		if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
			aco.log.Error(stdErrCloseErr, "Closing stderr log destination failed", "Container", container)
		}
	}()

	return nil
}

// NETWORK MANAGEMENT
//
// The Apple container runtime can attach containers to networks only when the container is
// created; there is no `network connect`/`network disconnect`. DCP's container and network
// controllers, however, create containers first and connect them to the requested networks
// afterwards. To bridge this gap, all DCP-managed networks are mapped onto the built-in
// "default" network that every container is attached to automatically:
//
//   - CreateNetwork does not create anything and returns the "default" network ID.
//   - ConnectNetwork verifies that the container is attached to the requested network.
//   - DisconnectNetwork is a no-op.
//
// Containers attached to the default network get their own routable IP address and can reach
// each other directly, so the connectivity expectations of DCP-managed networks still hold.

func (aco *AppleContainerCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := aco.networkEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (aco *AppleContainerCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	// Containers cannot be attached to networks after creation with the Apple container runtime,
	// so DCP-managed networks are mapped onto the built-in default network (see above).
	aco.log.V(1).Info("The Apple container runtime does not support connecting containers to networks after creation; using the built-in default network", "RequestedNetwork", options.Name)

	network, err := aco.inspectSingleNetwork(ctx, defaultNetworkName)
	if err != nil {
		return "", err
	}

	return network.Id, nil
}

func (aco *AppleContainerCliOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	var removed []string
	var err error
	var toRemove []string

	for _, network := range options.Networks {
		if network == defaultNetworkName {
			// The built-in default network cannot (and should not) be removed.
			// DCP-managed networks map onto it, so report success.
			removed = append(removed, network)
		} else {
			toRemove = append(toRemove, network)
		}
	}

	if len(toRemove) > 0 {
		args := append([]string{"network", "rm"}, toRemove...)
		cmd := makeAppleContainerCommand(args...)
		outBuf, errBuf, cmdErr := aco.runBufferedCommand(ctx, "RemoveNetworks", cmd, nil, nil, ordinaryCommandTimeout)
		if cmdErr != nil {
			err = errors.Join(cmdErr, normalizeCliErrors(errBuf, networkDeleteFailedErrorMatch.MaxObjects(len(toRemove)), newNetworkNotFoundErrorMatch.MaxObjects(len(toRemove))))
		}

		nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
		removed = append(removed, slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })...)
	}

	if len(removed) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v networks were successfully removed", len(removed), len(options.Networks))))
	}

	return removed, err
}

// InspectNetworks inspects networks one at a time (the Apple container CLI fails the whole
// inspection if any of the requested networks is missing). The list of attached containers
// is not part of the Apple network inspect output and is synthesized from the container list.
func (aco *AppleContainerCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	var inspected []containers.InspectedNetwork
	var err error

	for _, network := range options.Networks {
		net, inspectErr := aco.inspectSingleNetwork(ctx, network)
		if inspectErr != nil {
			err = errors.Join(err, inspectErr)
			continue
		}
		inspected = append(inspected, *net)
	}

	// Populate the attached containers for the networks we successfully inspected.
	if len(inspected) > 0 {
		if attachments, attachErr := aco.getNetworkAttachments(ctx); attachErr != nil {
			err = errors.Join(err, attachErr)
		} else {
			for i := range inspected {
				inspected[i].Containers = attachments[inspected[i].Name]
			}
		}
	}

	if len(inspected) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("only %v out of %v networks were successfully inspected", len(inspected), len(options.Networks))))
	}

	return inspected, err
}

func (aco *AppleContainerCliOrchestrator) inspectSingleNetwork(ctx context.Context, network string) (*containers.InspectedNetwork, error) {
	cmd := makeAppleContainerCommand("network", "inspect", network)
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "InspectNetworks", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return nil, errors.Join(err, normalizeCliErrors(errBuf, newNetworkNotFoundErrorMatch))
	}

	networks, unmarshalErr := asArrayOfObjects(outBuf, unmarshalNetwork)
	if unmarshalErr != nil {
		return nil, unmarshalErr
	}
	if len(networks) == 0 {
		return nil, errors.Join(containers.ErrNotFound, fmt.Errorf("network '%s' was not found", network))
	}

	return &networks[0], nil
}

// getNetworkAttachments returns the containers attached to each network, keyed by network name.
func (aco *AppleContainerCliOrchestrator) getNetworkAttachments(ctx context.Context) (map[string][]containers.InspectedNetworkContainer, error) {
	cmd := makeAppleContainerCommand("ls", "--all", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "ListContainers", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return nil, errors.Join(err, normalizeCliErrors(errBuf))
	}

	var listed []appleListedContainer
	if unmarshalErr := json.Unmarshal(outBuf.Bytes(), &listed); unmarshalErr != nil {
		return nil, errors.Join(containers.ErrUnmarshalling, unmarshalErr)
	}

	attachments := map[string][]containers.InspectedNetworkContainer{}
	for _, ctr := range listed {
		for _, network := range ctr.Configuration.Networks {
			attachments[network.Network] = append(attachments[network.Network], containers.InspectedNetworkContainer{
				Id:   ctr.Id,
				Name: ctr.Id,
			})
		}
	}

	return attachments, nil
}

// ConnectNetwork verifies that the container is attached to the requested network.
// The Apple container runtime cannot attach a container to a network after the container
// has been created, so if the container is not attached this returns an error.
func (aco *AppleContainerCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	inspected, err := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{options.Container}})
	if err != nil {
		return err
	}

	for _, network := range inspected[0].Networks {
		if network.Name == options.Network || network.Id == options.Network {
			// The container is already attached to the network (the only kind of "connection"
			// the Apple container runtime supports).
			return errors.Join(containers.ErrAlreadyExists, fmt.Errorf("container is already attached to network '%s'", options.Network))
		}
	}

	return fmt.Errorf("the Apple container runtime does not support connecting a container to a network after the container has been created")
}

// DisconnectNetwork is a no-op: the Apple container runtime does not support detaching
// a container from a network. DCP uses disconnection only as a preparation step before
// connecting a container to its requested networks, which maps onto the built-in default
// network for this runtime (see above), so there is nothing to do here.
func (aco *AppleContainerCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	aco.log.V(1).Info("The Apple container runtime does not support disconnecting containers from networks; ignoring", "Network", options.Network, "Container", options.Container)
	return nil
}

func (aco *AppleContainerCliOrchestrator) ListNetworks(ctx context.Context, options containers.ListNetworksOptions) ([]containers.ListedNetwork, error) {
	// Note: the Apple container CLI has no server-side label filtering; filters are applied client-side.
	cmd := makeAppleContainerCommand("network", "ls", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "ListNetworks", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return nil, errors.Join(err, normalizeCliErrors(errBuf))
	}

	listed, unmarshalErr := asArrayOfObjects(outBuf, unmarshalListedNetwork)
	if unmarshalErr != nil {
		return nil, unmarshalErr
	}

	return filterByLabels(listed, options.Filters.LabelFilters, func(ln containers.ListedNetwork) map[string]string { return ln.Labels }), nil
}

func (aco *AppleContainerCliOrchestrator) DefaultNetworkName() string {
	return defaultNetworkName
}

// UsesSingleNetwork reports that the Apple container runtime exposes only the built-in default
// network. DCP maps all networks onto it and skips the connect/disconnect machinery. See
// containers.SingleNetworkOrchestrator.
func (aco *AppleContainerCliOrchestrator) UsesSingleNetwork() bool {
	return true
}

// COMMAND EXECUTION HELPERS

type streamCommandOption uint32

const (
	streamCommandOptionNone       streamCommandOption = 0
	streamCommandOptionUseWatcher streamCommandOption = 1
)

func (aco *AppleContainerCliOrchestrator) streamCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdOutWriter io.Writer,
	stdErrWriter io.Writer,
	opts streamCommandOption,
) (<-chan error, error) {
	cmd.Stdout = stdOutWriter
	cmd.Stderr = stdErrWriter

	exitCh := make(chan error)
	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		defer close(exitCh)
		if err != nil {
			exitCh <- err
		}

		if exitCode != 0 {
			exitCh <- fmt.Errorf("apple container command '%s' returned with non-zero exit code %d", commandName, exitCode)
		}
	}

	aco.log.V(1).Info("Running Apple container command", "Command", cmd.String())
	handle, startWaitForProcessExit, err := aco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler), process.CreationFlagsNone, nil)
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start Apple container command '%s'", commandName))
	}

	if opts&streamCommandOptionUseWatcher != 0 {
		dcpproc.RunProcessWatcher(aco.executor, handle, aco.log)
	}

	startWaitForProcessExit()

	return exitCh, nil
}

func (aco *AppleContainerCliOrchestrator) runBufferedCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdOutWriteCloser io.WriteCloser,
	stdErrWriteCloser io.WriteCloser,
	timeout time.Duration,
) (*bytes.Buffer, *bytes.Buffer, error) {
	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outBuf := new(bytes.Buffer)
	var stdOutWriter io.Writer
	if stdOutWriteCloser != nil {
		defer func() { _ = stdOutWriteCloser.Close() }()
		stdOutWriter = io.MultiWriter(stdOutWriteCloser, outBuf)
	} else {
		stdOutWriter = outBuf
	}

	errBuf := new(bytes.Buffer)
	var stdErrWriter io.Writer
	if stdErrWriteCloser != nil {
		defer func() { _ = stdErrWriteCloser.Close() }()
		stdErrWriter = io.MultiWriter(stdErrWriteCloser, errBuf)
	} else {
		stdErrWriter = errBuf
	}

	exitCh, err := aco.streamCommand(effectiveCtx, commandName, cmd, stdOutWriter, stdErrWriter, streamCommandOptionNone)
	if err == nil {
		// If we successfully started running, wait for the command to finish
		exitErr := <-exitCh
		if exitErr != nil {
			err = exitErr
		}
	}

	return outBuf, errBuf, err
}

func makeAppleContainerCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("container", args...)
	// exec.Command resolves cmd.Path via LookPath but leaves cmd.Args[0] as
	// the original "container" string. Align Args[0] with the resolved Path so
	// downstream consumers and child-process
	// argv[0] observers see a consistent, fully-qualified command name.
	if cmd.Path != "" {
		cmd.Args[0] = cmd.Path
	}
	return cmd
}

func (aco *AppleContainerCliOrchestrator) MakeCommand(args ...string) *exec.Cmd {
	return makeAppleContainerCommand(args...)
}

func (aco *AppleContainerCliOrchestrator) RunBufferedCommand(ctx context.Context, opName string, cmd *exec.Cmd, stdout io.WriteCloser, stderr io.WriteCloser, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	return aco.runBufferedCommand(ctx, opName, cmd, stdout, stderr, timeout)
}

func normalizeCliErrors(errBuf *bytes.Buffer, errorMatches ...containers.ErrorMatch) error {
	errorMatches = append(errorMatches, runtimeNotRunningErrorMatch)
	return containers.NormalizeCliErrors(errBuf, errorMatches...)
}

// writeLayerToFile materializes an image layer (either a source file reference or inline
// base64 contents) as a tar file in the build context directory, verifying the SHA256 hash
// of source files.
func writeLayerToFile(layer *apiv1.ImageLayer, contextDir string, fileName string) error {
	targetPath := contextDir + string(os.PathSeparator) + fileName

	if layer.Source != "" {
		data, readErr := os.ReadFile(layer.Source)
		if readErr != nil {
			return fmt.Errorf("reading layer source file %q: %w", layer.Source, readErr)
		}

		if verifyErr := verifySha256(data, layer.SHA256); verifyErr != nil {
			return fmt.Errorf("verifying layer source file %q: %w", layer.Source, verifyErr)
		}

		return os.WriteFile(targetPath, data, 0644)
	}

	decoded, decodeErr := base64.StdEncoding.DecodeString(layer.RawContents)
	if decodeErr != nil {
		return fmt.Errorf("decoding base64 rawContents for layer (%q): %w", layer.Digest, decodeErr)
	}

	return os.WriteFile(targetPath, decoded, 0644)
}

var _ containers.VolumeOrchestrator = (*AppleContainerCliOrchestrator)(nil)
var _ containers.ImageOrchestrator = (*AppleContainerCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*AppleContainerCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*AppleContainerCliOrchestrator)(nil)
