/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

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
	"path/filepath"
	"regexp"
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
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/slices"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/internal/termpty"
)

var (
	// wslc reports "not found" errors on standard error with these shapes (exit code 1):
	//   container: Container '<id>' not found.
	//   network:   Network not found: '<name>'
	//   volume:    Volume not found: '<name>'
	containerNotFoundRegEx = regexp.MustCompile(`(?i)container '.*' not found`)
	networkNotFoundRegEx   = regexp.MustCompile(`(?i)network not found`)
	volumeNotFoundRegEx    = regexp.MustCompile(`(?i)volume not found`)
	// wslc reports a missing image as "Image '<ref>' not found." on inspect, and as
	// "Error code: WSLC_E_IMAGE_NOT_FOUND" (or "pull access denied ...") on pull.
	imageNotFoundRegEx = regexp.MustCompile(`(?i)(image '.*' not found|image not found|image_not_found|no such image)`)
	// Creating an object that already exists reports a Windows error code (e.g. ERROR_ALREADY_EXISTS)
	// or the underlying "... already exists" message.
	alreadyExistsRegEx = regexp.MustCompile(`(?i)error_already_exists|already exists`)
	// TODO: refine once the exact message emitted when the wslc service is unavailable is confirmed.
	runtimeNotHealthyRegEx = regexp.MustCompile(`(?i)(wsl.*is not running|could not connect|service is not running|the wsl service)`)

	// Extracts the version number from the single-line `wslc version` output, e.g. "wslc 2.9.3.0".
	versionRegEx = regexp.MustCompile(`(?i)wslc\s+([0-9][0-9.]*)`)
	// Matches the exec failure emitted when the container image lacks a `tar` binary, used to
	// produce a clearer diagnostic for in-container file injection (wslc has no `container cp`).
	tarMissingRegEx = regexp.MustCompile(`(?i)(tar)(:|.{0,20})(not found|no such file|executable file not found|command not found)`)

	newContainerNotFoundErrorMatch    = containers.NewCliErrorMatch(containerNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
	newVolumeNotFoundErrorMatch       = containers.NewCliErrorMatch(volumeNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
	newNetworkNotFoundErrorMatch      = containers.NewCliErrorMatch(networkNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
	imageNotFoundErrorMatch           = containers.NewCliErrorMatch(imageNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("image not found")))
	newNetworkAlreadyExistsErrorMatch = containers.NewCliErrorMatch(alreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("network already exists")))
	newVolumeAlreadyExistsErrorMatch  = containers.NewCliErrorMatch(alreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("volume already exists")))
	newWslcNotRunningErrorMatch       = containers.NewCliErrorMatch(runtimeNotHealthyRegEx, errors.Join(containers.ErrRuntimeNotHealthy, fmt.Errorf("wslc runtime is not healthy")))

	// We expect almost all wslc CLI invocations to finish within this time.
	ordinaryWslcCommandTimeout = 30 * time.Second

	// We allow up to a minute for diagnostic commands to finish as we'd rather wait a bit longer than miss information.
	diagnosticWslcCommandTimeout = 1 * time.Minute

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
)

type WslcCliOrchestrator struct {
	log logr.Logger

	// Process executor for running wslc commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]

	// Event watcher for network events
	networkEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]
}

func NewWslcCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	wco := &WslcCliOrchestrator{
		log:      log,
		executor: executor,
	}

	wco.containerEvtWatcher = pubsub.NewSubscriptionSet(wco.doWatchContainers, context.Background())
	wco.networkEvtWatcher = pubsub.NewSubscriptionSet(wco.doWatchNetworks, context.Background())

	return wco
}

func (*WslcCliOrchestrator) IsDefault() bool {
	return false
}

func (*WslcCliOrchestrator) Name() string {
	return "wslc"
}

func (*WslcCliOrchestrator) ContainerHost() string {
	// wslc does not resolve host.containers.internal; the Docker-style alias is available instead.
	return "host.docker.internal"
}

func (wco *WslcCliOrchestrator) CheckStatus(ctx context.Context, cacheUsage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
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
			// Timed out, assume wslc is not responsive and unavailable
			return containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     "Timed out while checking wslc status; wslc CLI is not responsive.",
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

	newStatus := wco.getStatus(ctx)
	updateStatus.Lock()
	// Update the cached status
	cachedStatus = &newStatus
	updateStatus.Unlock()

	return newStatus
}

// Check the status of the wslc runtime in the background until the context is canceled.
func (wco *WslcCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if !backgroundStatusUpdates.CompareAndSwap(0, 1) {
		return
	}

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		for {
			// Only one goroutine should be checking the status at a time
			if checkStatusLock.TryLock() {
				newStatus := wco.getStatus(ctx)

				updateStatus.Lock()
				// Update the cached status
				cachedStatus = &newStatus
				updateStatus.Unlock()
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

func (wco *WslcCliOrchestrator) getStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	cmd := makeWslcCommand("container", "list", "--last", "1", "--quiet")
	_, stdErr, err := wco.runBufferedWslcCommand(ctx, "Info", cmd, nil, nil, ordinaryWslcCommandTimeout)

	if errors.Is(err, exec.ErrNotFound) {
		// Try to get the inner error if this is an exec.ErrNotFound error
		if unwrapErr := errors.Unwrap(err); errors.Is(unwrapErr, exec.ErrNotFound) {
			err = unwrapErr
		}

		// Couldn't find the wslc CLI, so it's not installed
		return containers.ContainerRuntimeStatus{
			Installed: false,
			Running:   false,
			Error:     err.Error(),
		}
	} else if err != nil {
		var stdErrString string

		// Prefer returning any stderr from the runtime command, but if that is empty, use the error message from the error object.
		// The goal is to make it easy for users to diagnose underlying container runtime issues based on the error message.
		if stdErr != nil {
			stdErrString = strings.TrimSpace(stdErr.String())
		}

		if stdErrString == "" {
			stdErrString = err.Error()
		}

		// Error response from the wslc command, assume runtime isn't available
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Running:   false,
			Error:     stdErrString,
		}
	}

	// Info command returned successfully, assume runtime is ready
	return containers.ContainerRuntimeStatus{
		Installed: true,
		Running:   true,
	}
}

func (wco *WslcCliOrchestrator) GetDiagnostics(ctx context.Context) (containers.ContainerDiagnostics, error) {
	// wslc reports its version as a single plain-text line (e.g. "wslc 2.9.3.0") and does not
	// support a --format option, so the output is parsed with a regular expression.
	cmd := makeWslcCommand("version")
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "Version", cmd, nil, nil, diagnosticWslcCommandTimeout)
	if err != nil {
		return containers.ContainerDiagnostics{}, errors.Join(err, normalizeCliErrors(errBuf))
	}

	return parseDiagnostics(outBuf.Bytes())
}

func (wco *WslcCliOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	cmd := makeWslcCommand("volume", "create", options.Name)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "CreateVolume", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		return errors.Join(err, normalizeCliErrors(errBuf, newVolumeAlreadyExistsErrorMatch.MaxObjects(1)))
	}

	return containers.ExpectCliStrings(outBuf, []string{options.Name})
}

func (wco *WslcCliOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	// wslc inspect commands do not support a --format option and return JSON by default.
	cmd := makeWslcCommand(append([]string{"volume", "inspect"}, options.Volumes...)...)

	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "InspectVolumes", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(options.Volumes))))
	}

	inspectedVolumes, unmarshalErr := asObjects(outBuf, unmarshalVolume)
	err = errors.Join(err, unmarshalErr)

	if len(inspectedVolumes) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all volumes were inspected, expected %d but got %d", len(options.Volumes), len(inspectedVolumes))))
	}

	return inspectedVolumes, err
}

func (wco *WslcCliOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	args := []string{"volume", "remove"}
	if options.Force {
		args = append(args, "--force")
	}

	args = append(args, options.Volumes...)

	cmd := makeWslcCommand(args...)

	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "RemoveVolumes", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(options.Volumes))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(removed) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all volumes were removed, expected %d but got %d", len(options.Volumes), len(removed))))
	}

	return removed, err
}

func (wco *WslcCliOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	args := []string{"image", "build"}

	if options.Dockerfile != "" {
		args = append(args, "-f", options.Dockerfile)
	}

	// Should base images be updated even if they are already present locally?
	if options.Pull {
		args = append(args, "--pull")
	}

	// Apply all tags specified in the build context to the image
	for _, tag := range options.Tags {
		args = append(args, "-t", tag)
	}

	// Apply all specified build arguments
	for _, buildArg := range options.Args {
		if buildArg.Value != "" {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", buildArg.Name, buildArg.Value))
		} else {
			args = append(args, "--build-arg", buildArg.Name)
		}
	}

	// If a build stage is given, use it
	if options.Stage != "" {
		args = append(args, "--target", options.Stage)
	}

	// Apply any specified labels
	for _, label := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}

	// TODO: wslc build does not support build secrets, --iidfile, or --platform. Secrets and
	// platform selection are silently ignored; warn so the limitation is visible in logs.
	if len(options.Secrets) > 0 {
		wco.log.Info("wslc does not support build secrets; the secrets will not be available to the build")
	}
	if options.Platform != "" {
		wco.log.Info("wslc does not support selecting a build platform; building for the host platform", "Platform", options.Platform)
	}

	// Append the build context argument
	args = append(args, options.Context)

	cmd := makeWslcCommand(args...)

	// Building an image can take a long time to finish, particularly if any base images are not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultBuildImageTimeout
	}
	_, errBuf, err := wco.runBufferedWslcCommand(ctx, "BuildImage", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return errors.Join(err, normalizeCliErrors(errBuf))
	}

	// wslc has no --iidfile flag, so when the caller expects the built image ID to be written
	// to a file, resolve it from the first tag and write it ourselves.
	if options.IidFile != "" && len(options.Tags) > 0 {
		inspected, inspectErr := wco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{options.Tags[0]}})
		if inspectErr != nil || len(inspected) == 0 {
			wco.log.Error(inspectErr, "Could not resolve built image ID for the image ID file", "Tag", options.Tags[0])
		} else if writeErr := usvc_io.WriteFile(options.IidFile, []byte(inspected[0].Id), osutil.PermissionOwnerReadWriteOthersRead); writeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("could not write image ID file '%s'", options.IidFile))
		}
	}

	return nil
}

func (wco *WslcCliOrchestrator) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if len(options.Images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}

	var inspectedImages []containers.InspectedImage

	cmd := makeWslcCommand(append([]string{"image", "inspect"}, options.Images...)...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "InspectImages", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, imageNotFoundErrorMatch.MaxObjects(len(options.Images))))
	} else {
		inspectedImages, err = asObjects(outBuf, unmarshalImage)

		if len(inspectedImages) < len(options.Images) {
			err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all images were inspected, expected %d but got %d", len(options.Images), len(inspectedImages))))
		}
	}

	return inspectedImages, err
}

func (wco *WslcCliOrchestrator) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	if options.Image == "" {
		return "", fmt.Errorf("must specify an image to pull")
	}

	// wslc image pull has no flags; a specific digest is selected via the image reference.
	ref := options.Image
	if options.Digest != "" {
		ref = options.Image + "@" + options.Digest
	}

	cmd := makeWslcCommand("image", "pull", ref)
	// Pulling large images can take a long time, especially if the image is not available locally and the network is slow.
	if options.Timeout == 0 {
		options.Timeout = defaultPullImageTimeout
	}
	_, errBuf, err := wco.runBufferedWslcCommand(ctx, "PullImage", cmd, nil, nil, options.Timeout)
	if err != nil {
		return "", errors.Join(err, normalizeCliErrors(errBuf, imageNotFoundErrorMatch))
	}

	// wslc pull does not emit the image ID, so resolve it by inspecting the pulled reference.
	inspected, inspectErr := wco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{ref}})
	if inspectErr != nil || len(inspected) == 0 {
		// Best effort: return the reference if the image ID cannot be resolved.
		return ref, nil
	}

	return inspected[0].Id, nil
}

func applyCreateContainerOptions(args []string, options containers.CreateContainerOptions) []string {
	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	if len(options.CreationNetworks) > 0 {
		// wslc attaches networks only at creation time and supports repeating --network for multiple
		// networks, each followed by its own --network-alias flags.
		for _, network := range options.CreationNetworks {
			args = append(args, "--network", network.Name)
			for _, alias := range network.Aliases {
				args = append(args, "--network-alias", alias)
			}
		}
	} else if options.Network != "" {
		args = append(args, "--network", options.Network)

		if len(options.NetworkAliases) > 0 {
			for _, alias := range options.NetworkAliases {
				args = append(args, "--network-alias", alias)
			}
		}
	}

	// wslc uses Docker-style bind syntax for mounts; the --mount option is not supported.
	for _, mount := range options.VolumeMounts {
		mountVal := mount.Target
		if mount.Source != "" {
			mountVal = fmt.Sprintf("%s:%s", mount.Source, mount.Target)
		}
		if mount.ReadOnly {
			mountVal += ":ro"
		}
		args = append(args, "-v", mountVal)
	}

	for _, port := range options.Ports {
		portVal := fmt.Sprintf("%d", port.ContainerPort)

		if port.HostPort != 0 {
			portVal = fmt.Sprintf("%d:%s", port.HostPort, portVal)
		} else {
			portVal = fmt.Sprintf(":%s", portVal)
		}

		if port.HostIP != "" {
			portVal = fmt.Sprintf("%s:%s", port.HostIP, portVal)
		} else {
			// Bind to 127.0.0.1 for extra security, not to 0.0.0.0 (all interfaces, making it accessible from the outside)
			// IPv6 is not well supported with container networking, so we assume IPv4. We'll need to revisit this logic
			// if we start getting requests to support IPv6 container networking.
			portVal = fmt.Sprintf("%s:%s", networking.IPv4LocalhostDefaultAddress, portVal)
		}

		if port.Protocol != "" {
			// wslc only accepts lowercase protocol names (tcp/udp); the API uses uppercase (TCP/UDP).
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
		args = append(args, "--label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}

	// TODO: wslc create does not support restart policies, pull policies, or healthcheck
	// configuration, so those options are not applied here.

	if options.Command != "" {
		args = append(args, "--entrypoint", options.Command)
	}

	if options.Terminal != nil {
		// Attach a TTY (-t) and keep STDIN open (-i) if a terminal is requested.
		// wslc does not support bundled short flags, so they are passed separately.
		args = append(args, "-i", "-t")
	}

	args = append(args, options.RunArgs...)

	return args
}

func (wco *WslcCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	args := []string{"container", "create"}

	args = applyCreateContainerOptions(args, options)

	args = append(args, options.Image)

	if len(options.Args) > 0 {
		args = append(args, options.Args...)
	}

	cmd := makeWslcCommand(args...)

	// Create container can take a long time to finish if the image is not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultCreateContainerTimeout
	}

	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "CreateContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
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

func (wco *WslcCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	args := []string{"container", "run"}

	args = applyCreateContainerOptions(args, options.CreateContainerOptions)

	args = append(args, "--detach")
	args = append(args, options.Image)

	if len(options.Args) > 0 {
		args = append(args, options.Args...)
	}

	cmd := makeWslcCommand(args...)

	// The run container command can take a long time to finish if the image is not available locally.
	// So we use much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultRunContainerTimeout
	}

	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "RunContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return "", errors.Join(err, normalizeCliErrors(errBuf))
	}

	return asId(outBuf)
}

func (wco *WslcCliOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	args := []string{"container", "exec"}

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

	cmd := makeWslcCommand(args...)

	cmd.Stdout = options.StdOutStream
	cmd.Stderr = options.StdErrStream

	exitCh := make(chan int32)
	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		// We only care about the exit code, not the error. The only scenario where we should get an error
		// is if the context for an exec command is canceled during DCP shutdown, in which case that's expected.
		if err != nil && !errors.Is(err, context.Canceled) {
			wco.log.Error(err, "Unexpected error during container exec command", "Command", cmd.String())
		}
		exitCh <- exitCode
		close(exitCh)
	}

	wco.log.V(1).Info("Running wslc command", "Command", cmd.String())
	_, _, startWaitForProcessExit, err := wco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler), process.CreationFlagsNone, nil)
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start wslc command '%s'", "ExecContainer"))
	}
	startWaitForProcessExit()

	return exitCh, nil
}

// Run `wslc container attach <container>` on a freshly allocated pseudo-terminal.
func (wco *WslcCliOrchestrator) AttachContainer(ctx context.Context, options containers.AttachContainerOptions) (*termpty.PseudoTerminalProcess, error) {
	cmd := makeWslcCommand("container", "attach", options.Container)
	return termpty.StartProcessWithTerminal(ctx, wco.executor, &termpty.CommandSpec{
		Cmd:           cmd,
		CreationFlags: process.CreationFlagEnsureKillOnDispose,
		Cols:          options.Cols,
		Rows:          options.Rows,
	})
}

func (wco *WslcCliOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	args := []string{"container", "list", "--no-trunc"}

	for _, label := range options.Filters.LabelFilters {
		filter := fmt.Sprintf("label=%s", label.Key)

		if label.Value != "" {
			filter += "=" + label.Value
		}

		args = append(args, "--filter", filter)
	}

	args = append(args, "--format", "json")

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "ListContainers", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		return nil, errors.Join(err, normalizeCliErrors(errBuf))
	}

	return asObjects(outBuf, unmarshalListedContainer)
}

func (wco *WslcCliOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	cmd := makeWslcCommand(append([]string{"container", "inspect"}, options.Containers...)...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "InspectContainers", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers))))
	}

	inspectedContainers, unmarshalErr := asObjects(outBuf, unmarshalContainer)
	err = errors.Join(err, unmarshalErr)

	if len(inspectedContainers) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were inspected, expected %d but got %d", len(options.Containers), len(inspectedContainers))))
	}

	return inspectedContainers, err
}

func (wco *WslcCliOrchestrator) StartContainers(ctx context.Context, options containers.StartContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, options.Containers...)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "StartContainers", cmd, options.StdOutStream, options.StdErrStream, ordinaryWslcCommandTimeout)
	if err != nil {
		err = normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	started := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(started) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were started, expected %d but got %d", len(options.Containers), len(started))))
	}

	return started, err
}

func (wco *WslcCliOrchestrator) StopContainers(ctx context.Context, options containers.StopContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "stop"}
	var timeout time.Duration = ordinaryWslcCommandTimeout
	if options.SecondsToKill > 0 {
		args = append(args, "--time", fmt.Sprintf("%d", options.SecondsToKill))
		timeout = time.Duration(options.SecondsToKill)*time.Second + ordinaryWslcCommandTimeout
	}
	args = append(args, options.Containers...)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "StopContainers", cmd, nil, nil, timeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	stopped := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(stopped) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were stopped, expected %d but got %d", len(options.Containers), len(stopped))))
	}

	return stopped, err
}

func (wco *WslcCliOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	// wslc container remove does not support removing associated anonymous volumes (-v).
	args := []string{"container", "remove"}
	if options.Force {
		args = append(args, "--force")
	}
	args = append(args, options.Containers...)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "RemoveContainers", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(options.Containers))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(removed) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were removed, expected %d but got %d", len(options.Containers), len(removed))))
	}

	return removed, err
}

// CreateFilesRequiresRunningContainer reports that wslc can only inject files into a running container.
// The wslc CLI has no "copy into a created container" primitive; CreateFiles writes files by executing
// `tar` inside the container, which requires the container to be running.
func (wco *WslcCliOrchestrator) CreateFilesRequiresRunningContainer() bool {
	return true
}

func (wco *WslcCliOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	tarBytes, buildErr := containers.BuildCreateFilesTar(options, wco.log)
	if buildErr != nil {
		return buildErr
	}

	if tarBytes == nil {
		// Can happen if all ContinueOnError items fail
		return nil
	}

	// wslc has no `container cp` command, so the tar stream is extracted by running tar
	// inside the container. This requires `tar` to be present in the container image.
	cmd := makeWslcCommand("container", "exec", "-i", options.Container, "tar", "-xf", "-", "-C", "/")
	cmd.Stdin = bytes.NewReader(tarBytes)

	_, errBuf, err := wco.runBufferedWslcCommand(ctx, "CopyFile", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		return buildCreateFilesError(options.Container, errBuf.String(), err)
	}

	return nil
}

// buildCreateFilesError composes the error returned when extracting files into a container fails.
// A missing tar binary surfaces as an exec failure from the container, not as a container-not-found
// error, so a hint is added pointing at the most likely cause (file injection needs `tar` on PATH).
func buildCreateFilesError(container, stderr string, err error) error {
	normalizedErr := normalizeCliErrors(bytes.NewBufferString(stderr), newContainerNotFoundErrorMatch.MaxObjects(1))
	if !errors.Is(normalizedErr, containers.ErrNotFound) && tarMissingRegEx.MatchString(stderr) {
		return errors.Join(fmt.Errorf("could not inject files into container %s: the container image must include a `tar` binary on PATH (wslc has no `container cp` command, so files are extracted via in-container tar): %w", container, err), normalizedErr)
	}
	return errors.Join(err, normalizedErr)
}

// buildImageMutex serializes wslc image builds across the whole process. wslc (v2.9.3.0) mounts each
// build's context directory as a volume into the shared CLI session and caps mounts at 15 per session;
// concurrent builds also fail intermittently with a "Catastrophic failure"/E_UNEXPECTED error and leak
// their mounts, eventually exhausting the session. A successful build releases its mount, so serializing
// builds keeps mount usage bounded and avoids the concurrency failures.
var buildImageMutex sync.Mutex

// ApplyImageLayers builds a derived image by applying additional tar layers on top of a base image.
// Unlike Docker/Podman, the wslc `build` command does not support reading a build context from stdin
// (its sole positional argument is an on-disk context directory), so the build context (a generated
// Dockerfile plus the layer tars) is materialized into a temporary directory and removed afterwards.
func (wco *WslcCliOrchestrator) ApplyImageLayers(ctx context.Context, options containers.ApplyImageLayersOptions) (string, error) {
	if len(options.Layers) == 0 {
		return "", fmt.Errorf("at least one image layer must be specified")
	}

	// wslc build cannot read the build context from stdin, so a tag is required to identify the
	// resulting image without parsing the verbose build output.
	if options.Tag == "" {
		return "", fmt.Errorf("wslc requires a tag to build a derived image with image layers")
	}

	randomSuffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return "", fmt.Errorf("generating temporary build context name: %w", randErr)
	}

	contextDir, contextErr := usvc_io.CreateTempFolder(fmt.Sprintf("wslc-build-%s", string(randomSuffix)), 0700)
	if contextErr != nil {
		return "", fmt.Errorf("creating temporary build context directory: %w", contextErr)
	}
	defer func() {
		if removeErr := os.RemoveAll(contextDir); removeErr != nil {
			wco.log.Error(removeErr, "Could not remove temporary build context directory", "Directory", contextDir)
		}
	}()

	// Use a tag if available, otherwise fall back to the image ID for the FROM directive.
	baseImage := options.BaseImage.Id
	if len(options.BaseImage.Tags) > 0 {
		baseImage = options.BaseImage.Tags[0]
	}

	dockerfile := fmt.Sprintf("FROM %s\n", baseImage)
	for i := range options.Layers {
		layer := &options.Layers[i]
		layerFileName := fmt.Sprintf("layer%d.tar", i)

		if writeErr := writeImageLayerFile(filepath.Join(contextDir, layerFileName), layer); writeErr != nil {
			return "", fmt.Errorf("writing image layer %d to build context: %w", i, writeErr)
		}

		dockerfile += fmt.Sprintf("ADD %s /\n", layerFileName)
	}

	dockerfilePath := filepath.Join(contextDir, "Dockerfile")
	dockerfileFile, dockerfileErr := usvc_io.OpenFile(dockerfilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if dockerfileErr != nil {
		return "", fmt.Errorf("creating Dockerfile in build context: %w", dockerfileErr)
	}
	if _, writeErr := dockerfileFile.WriteString(dockerfile); writeErr != nil {
		_ = dockerfileFile.Close()
		return "", fmt.Errorf("writing Dockerfile in build context: %w", writeErr)
	}
	if closeErr := dockerfileFile.Close(); closeErr != nil {
		return "", fmt.Errorf("closing Dockerfile in build context: %w", closeErr)
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultBuildImageTimeout
	}

	cmd := makeWslcCommand("build", "-t", options.Tag, contextDir)
	// Serialize the build invocation; see buildImageMutex.
	buildImageMutex.Lock()
	_, errBuf, buildErr := wco.runBufferedWslcCommand(ctx, "ApplyImageLayers", cmd, nil, nil, timeout)
	buildImageMutex.Unlock()
	if buildErr != nil {
		return "", errors.Join(fmt.Errorf("building derived image with image layers"), buildErr, normalizeCliErrors(errBuf))
	}

	wco.log.V(1).Info("Built derived image with image layers", "ImageRef", options.Tag, "LayerCount", len(options.Layers))
	return options.Tag, nil
}

// writeImageLayerFile writes a single image layer's tar contents to destPath, sourcing the bytes
// from either the layer's on-disk Source file or its base64-encoded RawContents.
func writeImageLayerFile(destPath string, layer *apiv1.ImageLayer) error {
	destFile, openErr := usvc_io.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if openErr != nil {
		return openErr
	}
	defer destFile.Close()

	if layer.Source != "" {
		sourceFile, sourceErr := usvc_io.OpenFile(layer.Source, os.O_RDONLY, 0)
		if sourceErr != nil {
			return fmt.Errorf("opening layer source file %q: %w", layer.Source, sourceErr)
		}
		defer sourceFile.Close()

		if _, copyErr := io.Copy(destFile, sourceFile); copyErr != nil {
			return fmt.Errorf("copying layer source file %q: %w", layer.Source, copyErr)
		}
		return nil
	}

	decoded, decodeErr := base64.StdEncoding.DecodeString(layer.RawContents)
	if decodeErr != nil {
		return fmt.Errorf("decoding base64 rawContents: %w", decodeErr)
	}
	if _, writeErr := destFile.Write(decoded); writeErr != nil {
		return writeErr
	}
	return nil
}

func (wco *WslcCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := wco.containerEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (wco *WslcCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout usvc_io.WriteSyncerCloser, stderr usvc_io.WriteSyncerCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"container", "logs"}
	args = options.Apply(args)
	args = append(args, container)

	cmd := makeWslcCommand(args...)

	exitCh, err := wco.streamWslcCommand(ctx, "CaptureContainerLogs", cmd, stdout, stderr, streamCommandOptionUseWatcher)
	if err != nil {
		return err
	}

	go func() {
		// Wait for the command to finish and clean up any resources
		exitErr := <-exitCh
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) && !errors.Is(exitErr, context.DeadlineExceeded) {
			wco.log.Error(err, "Capturing container logs failed", "Container", container)
		}

		if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
			wco.log.Error(stdOutCloseErr, "Closing stdout log destination failed", "Container", container)
		}
		if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
			wco.log.Error(stdErrCloseErr, "Closing stderr log destination failed", "Container", container)
		}
	}()

	return nil
}

func (wco *WslcCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := wco.networkEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (wco *WslcCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	args := []string{"network", "create"}

	// TODO: wslc network create does not support enabling IPv6.
	if options.IPv6 {
		wco.log.Info("wslc does not support creating IPv6 networks; creating an IPv4 network instead", "Network", options.Name)
	}

	for key, value := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", key, value))
	}

	args = append(args, options.Name)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "CreateNetwork", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		return "", errors.Join(err, normalizeCliErrors(errBuf, newNetworkAlreadyExistsErrorMatch.MaxObjects(1)))
	}

	return asId(outBuf)
}

func (wco *WslcCliOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "remove"}
	if options.Force {
		args = append(args, "--force")
	}
	args = append(args, options.Networks...)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "RemoveNetworks", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks))))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[string](nonEmpty, func(bs []byte) string { return string(bs) })

	if len(removed) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all networks were removed, expected %d but got %d", len(options.Networks), len(removed))))
	}

	return removed, err
}

func (wco *WslcCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	cmd := makeWslcCommand(append([]string{"network", "inspect"}, options.Networks...)...)
	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "InspectNetworks", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		err = errors.Join(err, normalizeCliErrors(errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks))))
	}

	inspectedNetworks, unmarshalErr := asObjects(outBuf, unmarshalNetwork)
	err = errors.Join(err, unmarshalErr)

	if len(inspectedNetworks) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all networks were inspected, expected %d but got %d", len(options.Networks), len(inspectedNetworks))))
	}

	return inspectedNetworks, err
}

func (wco *WslcCliOrchestrator) ConnectNetwork(_ context.Context, options containers.ConnectNetworkOptions) error {
	// TODO: wslc cannot connect a running container to a network; networks can only be attached
	// at container creation time via --network/--network-alias. Report the connection as already
	// existing so callers (which treat ErrAlreadyExists as success) do not retry indefinitely.
	wco.log.V(1).Info("wslc does not support connecting a running container to a network", "Network", options.Network, "Container", options.Container)
	return errors.Join(containers.ErrAlreadyExists, fmt.Errorf("wslc attaches networks only at container creation time"))
}

func (wco *WslcCliOrchestrator) DisconnectNetwork(_ context.Context, options containers.DisconnectNetworkOptions) error {
	// TODO: wslc cannot disconnect a container from a network. Report the network as not found so
	// callers (which treat ErrNotFound as success) consider the container disconnected.
	wco.log.V(1).Info("wslc does not support disconnecting a container from a network", "Network", options.Network, "Container", options.Container)
	return errors.Join(containers.ErrNotFound, fmt.Errorf("wslc does not support disconnecting a container from a network"))
}

func (wco *WslcCliOrchestrator) ListNetworks(ctx context.Context, options containers.ListNetworksOptions) ([]containers.ListedNetwork, error) {
	// wslc network list does not support --filter, so label filtering is performed in Go below.
	cmd := makeWslcCommand("network", "list", "--format", "json")

	outBuf, errBuf, err := wco.runBufferedWslcCommand(ctx, "ListNetworks", cmd, nil, nil, ordinaryWslcCommandTimeout)
	if err != nil {
		return nil, errors.Join(err, normalizeCliErrors(errBuf))
	}

	listedNetworks, err := asObjects(outBuf, unmarshalListedNetwork)
	if err != nil {
		return nil, err
	}

	if len(options.Filters.LabelFilters) == 0 {
		return listedNetworks, nil
	}

	// wslc network list output does not include labels, so inspect the networks to obtain labels
	// and filter the result set in Go.
	names := slices.Map[string](listedNetworks, func(n containers.ListedNetwork) string { return n.Name })
	inspected, inspectErr := wco.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: names})
	if inspectErr != nil && len(inspected) == 0 {
		return nil, inspectErr
	}

	labelsByName := make(map[string]map[string]string, len(inspected))
	for i := range inspected {
		labelsByName[inspected[i].Name] = inspected[i].Labels
	}

	filtered := make([]containers.ListedNetwork, 0, len(listedNetworks))
	for i := range listedNetworks {
		if networkMatchesLabelFilters(labelsByName[listedNetworks[i].Name], options.Filters.LabelFilters) {
			network := listedNetworks[i]
			network.Labels = labelsByName[network.Name]
			filtered = append(filtered, network)
		}
	}

	return filtered, nil
}

func (*WslcCliOrchestrator) DefaultNetworkName() string {
	return "bridge"
}

// NetworksAttachedAtCreation reports true: wslc can attach a container to networks only at creation
// time (via --network/--network-alias); it cannot connect or disconnect a running container.
func (*WslcCliOrchestrator) NetworksAttachedAtCreation() bool {
	return true
}

// TODO: wslc has no `events` command, so container and network events cannot be streamed.
// The watchers block until cancellation so the subscription set treats them as alive while
// never delivering any events.
func (*WslcCliOrchestrator) doWatchContainers(watcherCtx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
	<-watcherCtx.Done()
}

func (*WslcCliOrchestrator) doWatchNetworks(watcherCtx context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
	<-watcherCtx.Done()
}

type streamWslcCommandOption uint32

const (
	streamCommandOptionNone       streamWslcCommandOption = 0
	streamCommandOptionUseWatcher streamWslcCommandOption = 1
)

func (wco *WslcCliOrchestrator) streamWslcCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdOutWriter io.Writer,
	stdErrWriter io.Writer,
	opts streamWslcCommandOption,
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
			exitCh <- fmt.Errorf("wslc command '%s' returned with non-zero exit code %d", commandName, exitCode)
		}
	}

	wco.log.V(1).Info("Running wslc command", "Command", cmd.String())
	pid, startTime, startWaitForProcessExit, err := wco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler), process.CreationFlagsNone, nil)
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start wslc command '%s'", commandName))
	}

	if opts&streamCommandOptionUseWatcher != 0 {
		dcpproc.RunProcessWatcher(wco.executor, pid, startTime, wco.log)
	}

	startWaitForProcessExit()

	return exitCh, nil
}

func (wco *WslcCliOrchestrator) runBufferedWslcCommand(
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

	exitCh, err := wco.streamWslcCommand(effectiveCtx, commandName, cmd, stdOutWriter, stdErrWriter, streamCommandOptionNone)
	if err == nil {
		// If we successfully started running, wait for the command to finish
		exitErr := <-exitCh
		if exitErr != nil {
			err = exitErr
		}
	}

	if err != nil {
		stderr := ""
		stdout := ""
		if errBuf.Len() > 0 {
			stderr = errBuf.String()
		}
		if outBuf.Len() > 0 {
			stdout = outBuf.String()
		}

		return outBuf, errBuf, fmt.Errorf("%w: command output: Stdout: '%s' Stderr: '%s'", err, stdout, stderr)
	}

	return outBuf, errBuf, nil
}

func makeWslcCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("wslc", args...)
	// exec.Command resolves cmd.Path via LookPath but leaves cmd.Args[0] as
	// the original "wslc" string. Align Args[0] with the resolved Path so
	// downstream consumers and child-process argv[0] observers see a
	// consistent, fully-qualified command name.
	if cmd.Path != "" {
		cmd.Args[0] = cmd.Path
	}
	return cmd
}

func (wco *WslcCliOrchestrator) MakeCommand(args ...string) *exec.Cmd {
	return makeWslcCommand(args...)
}

func (wco *WslcCliOrchestrator) RunBufferedCommand(ctx context.Context, opName string, cmd *exec.Cmd, stdout io.WriteCloser, stderr io.WriteCloser, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	return wco.runBufferedWslcCommand(ctx, opName, cmd, stdout, stderr, timeout)
}

// wslc returns a JSON array for the commands that we parse, so unmarshal the entire output as an array of values.
func asObjects[T any, V any](b *bytes.Buffer, unmarshalFn func(*V, *T) error) ([]T, error) {
	if b == nil {
		return nil, fmt.Errorf("the wslc command timed out without returning any data")
	}

	retval := []T{}

	var unmarshalRaw []V
	rawBytes := b.Bytes()
	err := json.Unmarshal(rawBytes, &unmarshalRaw)
	if err != nil {
		return nil, err
	}

	for i := range unmarshalRaw {
		rawValue := &unmarshalRaw[i]

		var obj T
		err = unmarshalFn(rawValue, &obj)
		if err != nil {
			return nil, err
		}

		retval = append(retval, obj)
	}

	return retval, nil
}

func asId(b *bytes.Buffer) (string, error) {
	if b == nil {
		return "", fmt.Errorf("the wslc command timed out without returning object identifier")
	}

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), osutil.LF()), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

func parseDiagnostics(data []byte) (containers.ContainerDiagnostics, error) {
	if data == nil {
		return containers.ContainerDiagnostics{}, fmt.Errorf("the wslc command timed out without returning diagnostics data")
	}

	match := versionRegEx.FindSubmatch(data)
	if match == nil {
		return containers.ContainerDiagnostics{}, fmt.Errorf("could not parse wslc version from output: %q", string(data))
	}

	version := string(match[1])
	// wslc is a single binary, so the client and server versions are the same.
	return containers.ContainerDiagnostics{
		ClientVersion: version,
		ServerVersion: version,
	}, nil
}

func networkMatchesLabelFilters(labels map[string]string, filters []containers.LabelFilter) bool {
	for _, filter := range filters {
		value, ok := labels[filter.Key]
		if !ok {
			return false
		}
		if filter.Value != "" && value != filter.Value {
			return false
		}
	}
	return true
}

func unmarshalVolume(wvi *wslcInspectedVolume, vol *containers.InspectedVolume) error {
	vol.Name = wvi.Name
	vol.Driver = wvi.Driver
	vol.CreatedAt = wvi.CreatedAt
	vol.Labels = wvi.Labels

	return nil
}

func unmarshalImage(wii *wslcInspectedImage, ic *containers.InspectedImage) error {
	ic.Id = wii.Id
	ic.Labels = wii.Config.Labels
	ic.Tags = wii.RepoTags
	if len(wii.RepoDigests) > 0 {
		ic.Digest = wii.RepoDigests[0]
	}

	return nil
}

func unmarshalListedContainer(wlc *wslcListedContainer, lc *containers.ListedContainer) error {
	lc.Id = wlc.Id
	lc.Name = wlc.Name
	lc.Image = wlc.Image
	lc.Status = wslcStateToStatus(wlc.State)

	return nil
}

func unmarshalContainer(wci *wslcInspectedContainer, ic *containers.InspectedContainer) error {
	ic.Id = wci.Id
	ic.Name = wci.Name
	ic.Image = wci.Image
	ic.CreatedAt = wci.Created
	ic.StartedAt = wci.State.StartedAt
	ic.FinishedAt = wci.State.FinishedAt
	ic.Status = wci.State.Status
	ic.ExitCode = wci.State.ExitCode
	ic.Error = wci.State.Error

	ic.Mounts = make([]apiv1.VolumeMount, len(wci.Mounts))
	for i, mount := range wci.Mounts {
		source := mount.Source
		if mount.Type == apiv1.NamedVolumeMount {
			source = mount.Name
		}

		ic.Mounts[i] = apiv1.VolumeMount{
			Type:     mount.Type,
			Source:   source,
			Target:   mount.Destination,
			ReadOnly: !mount.ReadWrite,
		}
	}

	// wslc reports the container port bindings at the top level of the inspect output.
	ic.Ports = make(containers.InspectedContainerPortMapping)
	for portAndProtocol, portBindings := range wci.Ports {
		if len(portAndProtocol) == 0 || len(portBindings) == 0 {
			continue // Skip ports that are published but not mapped to host.
		}
		ic.Ports[portAndProtocol] = portBindings
	}

	ic.Env = make(map[string]string)
	for _, envVar := range wci.Config.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 1 {
			ic.Env[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			ic.Env[parts[0]] = ""
		}
	}

	ic.Args = append(ic.Args, wci.Config.Entrypoint...)
	ic.Args = append(ic.Args, wci.Config.Cmd...)

	// wslc inspect output does not include a network ID, so it is left empty.
	for name, network := range wci.NetworkSettings.Networks {
		ic.Networks = append(
			ic.Networks,
			containers.InspectedContainerNetwork{
				Name:       name,
				IPAddress:  network.IPAddress,
				Gateway:    network.Gateway,
				MacAddress: network.MacAddress,
				Aliases:    network.Aliases,
			},
		)
	}

	ic.Labels = wci.Labels

	return nil
}

func unmarshalNetwork(wcn *wslcInspectedNetwork, net *containers.InspectedNetwork) error {
	// wslc CLI accepts only network names (not the hash Id) for inspect/remove/connect, so the
	// name is used as the DCP-facing network Id to keep all subsequent operations addressable.
	net.Id = wcn.Name
	net.Name = wcn.Name
	net.Driver = wcn.Driver
	net.Scope = wcn.Scope
	if net.Scope == "" {
		net.Scope = "local"
	}
	net.Labels = wcn.Labels
	net.Attachable = true
	net.Internal = wcn.Internal
	net.Ingress = false
	for i := range wcn.IPAM.Config {
		net.Subnets = append(net.Subnets, wcn.IPAM.Config[i].Subnet)
		net.Gateways = append(net.Gateways, wcn.IPAM.Config[i].Gateway)
	}

	return nil
}

func unmarshalListedNetwork(wln *wslcListedNetwork, net *containers.ListedNetwork) error {
	net.Name = wln.Name
	// See unmarshalNetwork: wslc addresses networks by name, so the name is used as the Id.
	net.ID = wln.Name
	net.Driver = wln.Driver

	return nil
}

// wslcStateToStatus maps the integer container state reported by `wslc container list` to the
// DCP container status. Confirmed values: 1=created, 2=running, 3=exited.
// TODO: wslc has no pause command and the integer values for paused/restarting/removing/dead
// states have not been observed; unknown values are reported as an empty status.
func wslcStateToStatus(state int) containers.ContainerStatus {
	switch state {
	case 1:
		return containers.ContainerStatusCreated
	case 2:
		return containers.ContainerStatusRunning
	case 3:
		return containers.ContainerStatusExited
	default:
		return containers.ContainerStatus("")
	}
}

// wslcEntrypoint normalizes the container entrypoint, which may be reported either as a single
// string or as an array of strings, into an array of strings.
type wslcEntrypoint []string

func (we *wslcEntrypoint) UnmarshalJSON(b []byte) error {
	var maybeArray []string
	arrayErr := json.Unmarshal(b, &maybeArray)
	if arrayErr != nil {
		var maybeString string
		stringErr := json.Unmarshal(b, &maybeString)
		if stringErr != nil {
			return fmt.Errorf("error parsing container inspect: Entrypoint is neither a string nor an array of strings")
		}

		// entrypoint was a string, normalize to an array of one value
		*we = wslcEntrypoint{maybeString}
		return nil
	}

	// entrypoint is an array of strings
	*we = wslcEntrypoint(maybeArray)
	return nil
}

// wslcInspectedVolume corresponds to data returned by `wslc volume inspect`.
type wslcInspectedVolume struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver,omitempty"`
	CreatedAt time.Time         `json:"CreatedAt,omitempty"`
	Labels    map[string]string `json:"Labels,omitempty"`
}

// wslcInspectedImage corresponds to data returned by `wslc image inspect`.
type wslcInspectedImage struct {
	Id          string                   `json:"Id"`
	Config      wslcInspectedImageConfig `json:"Config,omitempty"`
	RepoTags    []string                 `json:"RepoTags,omitempty"`
	RepoDigests []string                 `json:"RepoDigests,omitempty"`
}

type wslcInspectedImageConfig struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

// wslcListedContainer corresponds to data returned by `wslc container list --format json`.
type wslcListedContainer struct {
	Id    string `json:"Id"`
	Name  string `json:"Name,omitempty"`
	Image string `json:"Image,omitempty"`
	State int    `json:"State,omitempty"`
}

// wslcInspectedContainer corresponds to data returned by `wslc container inspect`.
// Unlike Docker/Podman, wslc reports the image, labels, and port bindings at the top level.
type wslcInspectedContainer struct {
	Id              string                                   `json:"Id"`
	Name            string                                   `json:"Name,omitempty"`
	Image           string                                   `json:"Image,omitempty"`
	Labels          map[string]string                        `json:"Labels,omitempty"`
	Created         time.Time                                `json:"Created,omitempty"`
	Config          wslcInspectedContainerConfig             `json:"Config,omitempty"`
	State           wslcInspectedContainerState              `json:"State,omitempty"`
	Mounts          []wslcInspectedContainerMount            `json:"Mounts,omitempty"`
	Ports           containers.InspectedContainerPortMapping `json:"Ports,omitempty"`
	NetworkSettings wslcInspectedContainerNetworkSettings    `json:"NetworkSettings,omitempty"`
}

type wslcInspectedContainerConfig struct {
	Env        []string       `json:"Env,omitempty"`
	Cmd        []string       `json:"Cmd,omitempty"`
	Entrypoint wslcEntrypoint `json:"Entrypoint,omitempty"`
}

type wslcInspectedContainerState struct {
	Status     containers.ContainerStatus `json:"Status,omitempty"`
	StartedAt  time.Time                  `json:"StartedAt,omitempty"`
	FinishedAt time.Time                  `json:"FinishedAt,omitempty"`
	ExitCode   int32                      `json:"ExitCode,omitempty"`
	Error      string                     `json:"Error,omitempty"`
}

type wslcInspectedContainerMount struct {
	Type        apiv1.VolumeMountType `json:"Type,omitempty"`
	Name        string                `json:"Name,omitempty"`
	Source      string                `json:"Source,omitempty"`
	Destination string                `json:"Destination,omitempty"`
	ReadWrite   bool                  `json:"RW,omitempty"`
}

type wslcInspectedContainerNetworkSettings struct {
	Networks map[string]wslcInspectedContainerNetworkSettingsNetwork `json:"Networks,omitempty"`
}

type wslcInspectedContainerNetworkSettingsNetwork struct {
	IPAddress  string   `json:"IPAddress,omitempty"`
	Gateway    string   `json:"Gateway,omitempty"`
	MacAddress string   `json:"MacAddress,omitempty"`
	Aliases    []string `json:"Aliases,omitempty"`
}

// wslcInspectedNetwork corresponds to data returned by `wslc network inspect`.
type wslcInspectedNetwork struct {
	Id       string                   `json:"Id"`
	Name     string                   `json:"Name"`
	Driver   string                   `json:"Driver,omitempty"`
	Internal bool                     `json:"Internal,omitempty"`
	Scope    string                   `json:"Scope,omitempty"`
	Labels   map[string]string        `json:"Labels,omitempty"`
	IPAM     wslcInspectedNetworkIPAM `json:"IPAM,omitempty"`
}

type wslcInspectedNetworkIPAM struct {
	Driver string                           `json:"Driver,omitempty"`
	Config []wslcInspectedNetworkIPAMConfig `json:"Config,omitempty"`
}

type wslcInspectedNetworkIPAMConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

// wslcListedNetwork corresponds to data returned by `wslc network list --format json`.
type wslcListedNetwork struct {
	Name   string `json:"Name"`
	Id     string `json:"Id"`
	Driver string `json:"Driver,omitempty"`
}

func normalizeCliErrors(errBuf *bytes.Buffer, errorMatches ...containers.ErrorMatch) error {
	errorMatches = append(errorMatches, newWslcNotRunningErrorMatch)
	return containers.NormalizeCliErrors(errBuf, errorMatches...)
}

var _ containers.VolumeOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.ImageOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*WslcCliOrchestrator)(nil)
