package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"

	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/dcpproc"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
)

var (
	volumeNotFoundRegEx        = regexp.MustCompile(`(?i)no such volume`)
	networkNotFoundRegEx       = regexp.MustCompile(`(?i)network (.*) not found`)
	containerNotFoundRegEx     = regexp.MustCompile(`(?i)no such container`)
	networkAlreadyExistsRegEx  = regexp.MustCompile(`(?i)network with name (.*) already exists`)
	endpointAlreadyExistsRegEx = regexp.MustCompile(`(?i)endpoint with (.*) already exists in network`)
	networkSubnetPoolFullRegEx = regexp.MustCompile(`(?i)could not find an available, non-overlapping (.*) address pool`)
	dockerNotRunningRegEx      = regexp.MustCompile(`(?i)error during connect:`)
	volumeInUseRegEx           = regexp.MustCompile(`(?i)volume is in use`)

	newContainerNotFoundErrorMatch     = containers.NewCliErrorMatch(containerNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
	newVolumeNotFoundErrorMatch        = containers.NewCliErrorMatch(volumeNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
	volumeInUseErrorMatch              = containers.NewCliErrorMatch(volumeInUseRegEx, errors.Join(containers.ErrObjectInUse, fmt.Errorf("volume is being used by a container")))
	newNetworkNotFoundErrorMatch       = containers.NewCliErrorMatch(networkNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
	newNetworkAlreadyExistsErrorMatch  = containers.NewCliErrorMatch(networkAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("network already exists")))
	newEndpointAlreadyExistsErrorMatch = containers.NewCliErrorMatch(endpointAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("container already attached")))
	newNetworkPoolNotAvailableError    = containers.NewCliErrorMatch(networkSubnetPoolFullRegEx, errors.Join(containers.ErrCouldNotAllocate, fmt.Errorf("network subnet pool full")))
	newDockerNotRunningError           = containers.NewCliErrorMatch(dockerNotRunningRegEx, errors.Join(containers.ErrRuntimeNotHealthy, fmt.Errorf("docker runtime is not healthy")))

	// We expect almost all Docker CLI invocations to finish within this time.
	// Telemetry shows there is a very long tail for Docker command completion times, so we use a conservative default.
	ordinaryDockerCommandTimeout = 30 * time.Second

	defaultBuildImageTimeout      = 10 * time.Minute
	defaultCreateContainerTimeout = 10 * time.Minute
	defaultRunContainerTimeout    = 10 * time.Minute

	// Cache and synchronization control for checking runtime cachedStatus
	cachedStatus *containers.ContainerRuntimeStatus
	// Ensure that only one goroutine is checking the status at a time
	checkStatusSyncCh = concurrency.NewSyncChannel()
	// Mutex to control read/write access to the cached status
	updateStatus            = &sync.RWMutex{}
	backgroundStatusUpdates atomic.Int32
)

type DockerCliOrchestrator struct {
	log logr.Logger

	// Process executor for running Docker commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]

	// Event watcher for network events
	networkEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]
}

func NewDockerCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	dco := &DockerCliOrchestrator{
		log:      log,
		executor: executor,
	}

	dco.containerEvtWatcher = pubsub.NewSubscriptionSet(dco.doWatchContainers, context.Background())
	dco.networkEvtWatcher = pubsub.NewSubscriptionSet(dco.doWatchNetworks, context.Background())

	return dco
}

func (*DockerCliOrchestrator) IsDefault() bool {
	return true
}

func (*DockerCliOrchestrator) Name() string {
	return "docker"
}

func (*DockerCliOrchestrator) ContainerHost() string {
	return "host.docker.internal"
}

func (dco *DockerCliOrchestrator) CheckStatus(ctx context.Context, cacheUsage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	// A cached status is already available, return it
	updateStatus.RLock()
	if cachedStatus != nil && cacheUsage == containers.CachedRuntimeStatusAllowed {
		updateStatus.RUnlock()
		return *cachedStatus
	}
	updateStatus.RUnlock()

	if cacheUsage == containers.CachedRuntimeStatusAllowed {
		// For cached results, only one goroutine should be checking the status at a time
		if syncErr := checkStatusSyncCh.Lock(ctx); syncErr != nil {
			// Timed out, assume Docker is not responsive and unavailable
			return containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     "Timed out while checking Docker status; Docker CLI is not responsive.",
			}
		}

		defer checkStatusSyncCh.Unlock()
	}

	updateStatus.RLock()
	// Check again if the status is available in the cache
	if cachedStatus != nil && cacheUsage == containers.CachedRuntimeStatusAllowed {
		updateStatus.RUnlock()
		return *cachedStatus
	}
	updateStatus.RUnlock()

	newStatus := dco.getStatus(ctx)

	updateStatus.Lock()
	// Update the cached status
	cachedStatus = &newStatus
	updateStatus.Unlock()

	return newStatus
}

// Check the status of the Docker runtime in the background until the context is canceled.
func (dco *DockerCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if !backgroundStatusUpdates.CompareAndSwap(0, 1) {
		return
	}

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		for {
			// Only one goroutine should be checking the status at a time
			if checkStatusSyncCh.TryLock() {
				newStatus := dco.getStatus(ctx)

				updateStatus.Lock()
				// Update the cached status
				cachedStatus = &newStatus
				updateStatus.Unlock()

				checkStatusSyncCh.Unlock()
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

func (dco *DockerCliOrchestrator) getStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	// Check the status of the Docker runtime
	statusCh := make(chan containers.ContainerRuntimeStatus, 1)
	go func() {
		defer close(statusCh)
		// Run a simple command to check if the Docker CLI is installed and responsive
		cmd := makeDockerCommand("container", "ls", "-l")
		_, stdErr, err := dco.runBufferedDockerCommand(ctx, "Status", cmd, nil, nil, ordinaryDockerCommandTimeout)

		if errors.Is(err, exec.ErrNotFound) {
			// Try to get the inner error if this is an exec.ErrNotFound error
			if unwrapErr := errors.Unwrap(err); errors.Is(unwrapErr, exec.ErrNotFound) {
				err = unwrapErr
			}

			// Couldn't find the Docker CLI, so it's not installed
			statusCh <- containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     err.Error(),
			}

			return
		} else if errors.Is(err, context.DeadlineExceeded) {
			// Timed out, assume Docker is not responsive but available
			statusCh <- containers.ContainerRuntimeStatus{
				Installed: true,
				Running:   false,
				Error:     "Docker CLI timed out while checking status. Ensure Docker CLI is functioning correctly and try again.",
			}

			return
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

			// Error response from the Docker command, assume runtime isn't available
			statusCh <- containers.ContainerRuntimeStatus{
				Installed: true,
				Running:   false,
				Error:     stdErrString,
			}

			return
		}

		// Info command returned successfully, assume runtime is ready
		statusCh <- containers.ContainerRuntimeStatus{
			Installed: true,
			Running:   true,
		}
	}()

	// Some users create a "docker" alias to different runtimes, try to detect that case
	isDockerCh := make(chan bool, 1)
	go func() {
		defer close(isDockerCh)

		cmd := makeDockerCommand("info", "--format", "{{json .}}")
		outBuf, _, err := dco.runBufferedDockerCommand(ctx, "AliasDetection", cmd, nil, nil, ordinaryDockerCommandTimeout)
		if err != nil || outBuf == nil {
			// Failed to run the command, or got no output. This is probably not a valid Docker runtime.
			isDockerCh <- false
			return
		}

		var info dockerInfo
		if err = json.Unmarshal(outBuf.Bytes(), &info); err != nil {
			// Didn't get valid JSON, this is probably not a valid Docker runtime.
			isDockerCh <- false
			return
		}

		if info.DockerRootDir == "" {
			// Docker info should include DockerRootDir, this is probably a different runtime.
			isDockerCh <- false
			return
		}

		// Looks like a valid Docker runtime
		isDockerCh <- true
	}()

	newStatus := <-statusCh
	isDocker := <-isDockerCh

	if newStatus.Installed && newStatus.Running && !isDocker {
		newStatus.Installed = false
		newStatus.Running = false
		newStatus.Error = "docker command appears to be aliased to a different container runtime"
	}

	return newStatus
}

func (dco *DockerCliOrchestrator) CreateVolume(ctx context.Context, name string) error {
	cmd := makeDockerCommand("volume", "create", name)
	outBuf, _, err := dco.runBufferedDockerCommand(ctx, "CreateVolume", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		// Note: unlike Podman, Docker does not return an error if the volume already exists.
		return err
	}
	return containers.ExpectCliStrings(outBuf, []string{name})
}

func (dco *DockerCliOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	if len(volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	cmd := makeDockerCommand(append(
		[]string{"volume", "inspect", "--format", "{{json .}}"},
		volumes...)...,
	)

	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "InspectVolumes", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)))
	}

	return asObjects(outBuf, unmarshalVolume)
}

func (dco *DockerCliOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, volumes...)
	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "RemoveVolumes", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)), volumeInUseErrorMatch.MaxObjects(len(volumes)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, volumes)
}

func (dco *DockerCliOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	args := []string{"build"}

	if options.Dockerfile != "" {
		args = append(args, "-f", options.Dockerfile)
	}

	// Should base images be updated even if they are already present locally?
	if options.Pull {
		args = append(args, "--pull")
	}

	// If specified, the ID of the image will be written to this file
	if options.IidFile != "" {
		args = append(args, "--iidfile", options.IidFile)
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
				args = append(args, "--secret", fmt.Sprintf("id=%s", secret.ID))
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
		args = append(args, "--label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}

	// Enable plain output mode (Docker only)
	args = append(args, "--progress", "plain")

	// Append the build context argument
	args = append(args, options.Context)

	cmd := makeDockerCommand(args...)

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
	_, _, err := dco.runBufferedDockerCommand(ctx, "BuildImage", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return err
	}

	return nil
}

func (dco *DockerCliOrchestrator) InspectImages(ctx context.Context, images []string) ([]containers.InspectedImage, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}

	cmd := makeDockerCommand(append(
		[]string{"image", "inspect", "--format", "{{json .}}"},
		images...)...,
	)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "InspectImages", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(images)))
	}

	return asObjects(outBuf, unmarshalImage)
}

func applyCreateContainerOptions(args []string, options apiv1.ContainerSpec) []string {
	for _, mount := range options.VolumeMounts {
		mountVal := fmt.Sprintf("type=%s,src=%s,target=%s", mount.Type, mount.Source, mount.Target)
		if mount.ReadOnly {
			mountVal += ",readonly"
		}
		args = append(args, "--mount", mountVal)
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
			portVal = fmt.Sprintf("%s/%s", portVal, port.Protocol)
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

	if options.RestartPolicy != "" && options.RestartPolicy != apiv1.RestartPolicyNone {
		args = append(args, fmt.Sprintf("--restart=%s", options.RestartPolicy))
	}

	if options.PullPolicy != "" {
		args = append(args, "--pull", string(options.PullPolicy))
	}

	if options.Command != "" {
		args = append(args, "--entrypoint", options.Command)
	}

	args = append(args, options.RunArgs...)

	return args
}

func (dco *DockerCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	args := []string{"create"}

	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	if options.Network != "" {
		args = append(args, "--network", options.Network)
	}

	args = applyCreateContainerOptions(args, options.ContainerSpec)

	args = append(args, options.Image)

	if (options.Args != nil) && (len(options.Args) > 0) {
		args = append(args, options.Args...)
	}

	cmd := makeDockerCommand(args...)

	// Create container can take a long time to finish if the image is not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultCreateContainerTimeout
	}

	outBuf, _, err := dco.runBufferedDockerCommand(ctx, "CreateContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		if id, err2 := asId(outBuf); err2 == nil {
			// We got an ID, so the container was created, but the command failed.
			return id, err
		}

		return "", err
	}

	return asId(outBuf)
}

func (dco *DockerCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	args := []string{"run"}

	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	if options.Network != "" {
		args = append(args, "--network", options.Network)
	}

	args = applyCreateContainerOptions(args, options.ContainerSpec)

	args = append(args, "--detach")
	args = append(args, options.Image)

	if (options.Args != nil) && (len(options.Args) > 0) {
		args = append(args, options.Args...)
	}

	cmd := makeDockerCommand(args...)

	// The run container command can take a long time to finish if the image is not available locally.
	// So we use much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultRunContainerTimeout
	}

	outBuf, _, err := dco.runBufferedDockerCommand(ctx, "RunContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return "", err
	}
	return asId(outBuf)
}

func (dco *DockerCliOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
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

	cmd := makeDockerCommand(args...)

	cmd.Stdout = options.StdOutStream
	cmd.Stderr = options.StdErrStream

	exitCh := make(chan int32)
	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		// We only care about the exit code, not the error. The only scenario where we should get an error
		// is if the context for an exec command is canceled during DCP shutdown, in which case that's expected.
		if !errors.Is(err, context.Canceled) {
			dco.log.Error(err, "unexpected error during container exec command", "Command", cmd.String())
		}
		exitCh <- exitCode
		close(exitCh)
	}

	dco.log.V(1).Info("Running Docker command", "Command", cmd.String())
	_, _, startWaitForProcessExit, err := dco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler))
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start Docker command '%s'", "ExecContainer"))
	}
	startWaitForProcessExit()

	return exitCh, nil
}

func (dco *DockerCliOrchestrator) InspectContainers(ctx context.Context, names []string) ([]containers.InspectedContainer, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	cmd := makeDockerCommand(append(
		[]string{"container", "inspect", "--format", "{{json .}}"},
		names...)...,
	)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "InspectContainers", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	return asObjects(outBuf, unmarshalContainer)
}

func (dco *DockerCliOrchestrator) StartContainers(ctx context.Context, containerIDs []string, streamOptions containers.StreamCommandOptions) ([]string, error) {
	if len(containerIDs) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, containerIDs...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "StartContainers", cmd, streamOptions.StdOutStream, streamOptions.StdErrStream, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(containerIDs)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	started := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return started, containers.ExpectCliStrings(outBuf, containerIDs)
}

func (dco *DockerCliOrchestrator) StopContainers(ctx context.Context, names []string, secondsToKill uint) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "stop"}
	var timeout time.Duration = ordinaryDockerCommandTimeout
	if secondsToKill > 0 {
		args = append(args, "--time", fmt.Sprintf("%d", secondsToKill))
		timeout = time.Duration(secondsToKill)*time.Second + ordinaryDockerCommandTimeout
	}
	args = append(args, names...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "StopContainers", cmd, nil, nil, timeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	stopped := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return stopped, containers.ExpectCliStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) RemoveContainers(ctx context.Context, names []string, force bool) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "rm", "-v"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, names...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "RemoveContainers", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	args := []string{"container", "cp"}

	// Read the tar file to copy from standard input
	args = append(args, "-a", "-")

	args = append(args, options.Container+":"+options.Destination)

	tarWriter := usvc_io.NewTarWriter()

	for _, item := range options.Entries {
		if item.Type == apiv1.FileSystemEntryTypeDir {
			if addDirectoryErr := containers.AddDirectoryToTar(tarWriter, ".", options.DefaultOwner, options.DefaultGroup, options.Mode, item, options.ModTime); addDirectoryErr != nil {
				return addDirectoryErr
			}
		} else {
			if addFileErr := containers.AddFileToTar(tarWriter, ".", options.DefaultOwner, options.DefaultGroup, options.Mode, item, options.ModTime); addFileErr != nil {
				return addFileErr
			}
		}
	}

	buffer, bufferErr := tarWriter.Buffer()
	if bufferErr != nil {
		return bufferErr
	}

	cmd := makeDockerCommand(args...)
	cmd.Stdin = buffer
	_, errBuf, err := dco.runBufferedDockerCommand(ctx, "CopyFile", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf)
	}

	return nil
}

func (dco *DockerCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := dco.containerEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (dco *DockerCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout io.WriteCloser, stderr io.WriteCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"container", "logs"}
	args = options.Apply(args)
	args = append(args, container)

	cmd := makeDockerCommand(args...)

	exitCh, err := dco.streamDockerCommand(ctx, "CaptureContainerLogs", cmd, stdout, stderr, streamCommandOptionUseWatcher)
	if err != nil {
		return err
	}

	go func() {
		// Wait for the command to finish and clean up any resources
		exitErr := <-exitCh
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) && !errors.Is(exitErr, context.DeadlineExceeded) {
			dco.log.Error(err, "capturing container logs failed", "Container", container)
		}

		if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
			dco.log.Error(stdOutCloseErr, "closing stdout log destination failed", "Container", container)
		}
		if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
			dco.log.Error(stdErrCloseErr, "closing stderr log destination failed", "Container", container)
		}
	}()

	return nil
}

func (dco *DockerCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := dco.networkEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (dco *DockerCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	args := []string{"network", "create"}

	if options.IPv6 {
		args = append(args, "--ipv6")
	}

	for key, value := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", key, value))
	}

	args = append(args, options.Name)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "CreateNetwork", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return "", normalizeCliErrors(err, errBuf, newNetworkAlreadyExistsErrorMatch.MaxObjects(1), newNetworkPoolNotAvailableError)
	}
	return asId(outBuf)
}

func (dco *DockerCliOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "rm"}
	if options.Force {
		args = append(args, "--force")
	}
	args = append(args, options.Networks...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "RemoveNetworks", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, options.Networks)
}

func (dco *DockerCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "inspect", "--format", "{{json .}}"}
	args = append(args, options.Networks...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "InspectNetworks", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
	}

	return asObjects(outBuf, unmarshalNetwork)
}

func (dco *DockerCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	args := []string{"network", "connect"}

	for i := range options.Aliases {
		args = append(args, "--alias", options.Aliases[i])
	}

	args = append(args, options.Network, options.Container)

	cmd := makeDockerCommand(args...)
	_, errBuf, err := dco.runBufferedDockerCommand(ctx, "ConnectNetwork", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1), newEndpointAlreadyExistsErrorMatch)
	}
	return nil
}

func (dco *DockerCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	args := []string{"network", "disconnect"}

	if options.Force {
		args = append(args, "--force")
	}

	args = append(args, options.Network, options.Container)

	cmd := makeDockerCommand(args...)
	_, errBuf, err := dco.runBufferedDockerCommand(ctx, "DisconnectNetwork", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
	}
	return nil
}

func (dco *DockerCliOrchestrator) ListNetworks(ctx context.Context) ([]containers.ListedNetwork, error) {
	cmd := makeDockerCommand("network", "ls", "--format", "{{json .}}")
	outBuf, errBuf, err := dco.runBufferedDockerCommand(ctx, "ListNetworks", cmd, nil, nil, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf)
	}

	return asObjects(outBuf, unmarshalListedNetwork)
}

func (dco *DockerCliOrchestrator) DefaultNetworkName() string {
	return "bridge"
}

func (dco *DockerCliOrchestrator) doWatchContainers(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	args := []string{"events", "--filter", "type=container", "--format", "{{json .}}"}
	cmd := makeDockerCommand(args...)

	reader, writer := usvc_io.NewBufferedPipe()
	cmd.Stdout = writer
	defer writer.Close() // Ensure that the following scanner goroutine ends.

	scanner := bufio.NewScanner(reader)
	scanner.Buffer([]byte{}, 2048*1024) // Default max Scanner buffer is only 65k, which might not be enough for Docker events

	go func() {
		for scanner.Scan() {
			if watcherCtx.Err() != nil {
				return // Cancellation has been requested, so we should stop scanning events
			}

			evtData := scanner.Text()
			var evtMessage containers.EventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				dco.log.Error(unmarshalErr, "container event data could not be parsed", "EventData", evtData)
			} else {
				ss.Notify(evtMessage)
			}
		}

		if scanner.Err() != nil {
			dco.log.Error(scanner.Err(), "scanning for container events resulted in an error")
		}
	}()

	pic := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pic)

	// Container events are delivered on best-effort basis.
	// If the "docker events" command fails unexpectedly, we will log the error,
	// but we won't try to restart it.
	pid, startTime, startWaitForProcessExit, err := dco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		dco.log.Error(err, "could not execute 'docker events' command; container events unavailable")
		return
	}

	dcpproc.RunWatcher(dco.executor, pid, startTime, dco.log)

	startWaitForProcessExit()

	var exitInfo process.ProcessExitInfo
	select {
	case exitInfo = <-pic:
		if exitInfo.Err != nil {
			dco.log.Error(err, "'docker events' command failed")
		}
	case <-watcherCtx.Done():
		// We are asked to shut down
		dco.log.V(1).Info("stopping 'docker events' command", "pid", pid)
	}
}

func (dco *DockerCliOrchestrator) doWatchNetworks(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	args := []string{"events", "--filter", "type=network", "--format", "{{json .}}"}
	cmd := makeDockerCommand(args...)

	reader, writer := usvc_io.NewBufferedPipe()
	cmd.Stdout = writer
	defer writer.Close() // Ensure that the following scanner goroutine ends.

	scanner := bufio.NewScanner(reader)
	scanner.Buffer([]byte{}, 2048*1024) // Default max Scanner buffer is only 65k, which might not be enough for Docker events

	go func() {
		for scanner.Scan() {
			if watcherCtx.Err() != nil {
				return // Cancellation has been requested, so we should stop scanning events
			}

			var evtMessage containers.EventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				dco.log.Error(unmarshalErr, "network event data could not be parsed", "EventData", scanner.Text())
			} else {
				ss.Notify(evtMessage)
			}
		}

		if scanner.Err() != nil {
			dco.log.Error(scanner.Err(), "scanning for network events resulted in an error")
		}
	}()

	pic := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pic)

	// Container events are delivered on best-effort basis.
	// If the "docker events" command fails unexpectedly, we will log the error,
	// but we won't try to restart it.
	pid, startTime, startWaitForProcessExit, err := dco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		dco.log.Error(err, "could not execute 'docker events' command; network events unavailable")
		return
	}

	dcpproc.RunWatcher(dco.executor, pid, startTime, dco.log)

	startWaitForProcessExit()

	var exitInfo process.ProcessExitInfo
	select {
	case exitInfo = <-pic:
		if exitInfo.Err != nil {
			dco.log.Error(err, "'docker events' command failed")
		}
	case <-watcherCtx.Done():
		// We are asked to shut down
		dco.log.V(1).Info("stopping 'docker events' command", "pid", pid)
	}
}

type streamDockerCommandOption uint32

const (
	streamCommandOptionNone       streamDockerCommandOption = 0
	streamCommandOptionUseWatcher streamDockerCommandOption = 1
)

func (dco *DockerCliOrchestrator) streamDockerCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdOutWriter io.Writer,
	stdErrWriter io.Writer,
	opts streamDockerCommandOption,
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
			exitCh <- fmt.Errorf("docker command '%s' returned with non-zero exit code %d", commandName, exitCode)
		}
	}

	dco.log.V(1).Info("Running Docker command", "Command", cmd.String())
	pid, startTime, startWaitForProcessExit, err := dco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler))
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start Docker command '%s'", commandName))
	}

	if opts&streamCommandOptionUseWatcher != 0 {
		dcpproc.RunWatcher(dco.executor, pid, startTime, dco.log)
	}

	startWaitForProcessExit()

	return exitCh, nil
}

func (dco *DockerCliOrchestrator) runBufferedDockerCommand(
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

	exitCh, err := dco.streamDockerCommand(effectiveCtx, commandName, cmd, stdOutWriter, stdErrWriter, streamCommandOptionNone)
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

func makeDockerCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	return cmd
}

// Docker CLI returns JSON lines for most output, so split the lines before unmarshaling.
func asObjects[T any](b *bytes.Buffer, unmarshalFn func([]byte, *T) error) ([]T, error) {
	if b == nil {
		return nil, fmt.Errorf("the Docker command timed out without returning any data")
	}

	retval := []T{}
	chunks := bytes.Split(b.Bytes(), osutil.LF())

	for _, chunk := range chunks {
		chunk = bytes.TrimSpace(chunk)
		if len(chunk) == 0 {
			continue
		}

		var obj T
		err := unmarshalFn(chunk, &obj)
		if err != nil {
			return nil, err
		} else {
			retval = append(retval, obj)
		}
	}

	return retval, nil
}

func asId(b *bytes.Buffer) (string, error) {
	if b == nil {
		return "", fmt.Errorf("the Docker command timed out without returning object identifier")
	}

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), osutil.LF()), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

func unmarshalVolume(data []byte, vol *containers.InspectedVolume) error {
	if data == nil {
		return fmt.Errorf("the Docker command timed out without returning volume data")
	}

	return json.Unmarshal(data, vol)
}

func unmarshalImage(data []byte, ic *containers.InspectedImage) error {
	if data == nil {
		return fmt.Errorf("the Docker command timed out without returning image data")
	}

	var dii dockerInspectedImage
	err := json.Unmarshal(data, &dii)
	if err != nil {
		return err
	}

	ic.Id = dii.Id
	ic.Labels = dii.Config.Labels

	return nil
}

func unmarshalContainer(data []byte, ic *containers.InspectedContainer) error {
	if data == nil {
		return fmt.Errorf("the Docker command timed out without returning container data")
	}

	var dci dockerInspectedContainer
	err := json.Unmarshal(data, &dci)
	if err != nil {
		return err
	}

	ic.Id = dci.Id
	ic.Name = dci.Name
	ic.Image = dci.Config.Image
	ic.CreatedAt = dci.Created
	ic.StartedAt = dci.State.StartedAt
	ic.FinishedAt = dci.State.FinishedAt
	ic.Status = dci.State.Status
	ic.ExitCode = dci.State.ExitCode
	ic.Error = dci.State.Error

	ic.Mounts = make([]apiv1.VolumeMount, len(dci.Mounts))
	for i, mount := range dci.Mounts {
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

	ic.Ports = make(containers.InspectedContainerPortMapping)
	for portAndProtocol, portBindings := range dci.NetworkSettings.Ports {
		if len(portAndProtocol) == 0 || len(portBindings) == 0 {
			continue // Skip ports that are published but not mapped to host.
		}
		ic.Ports[portAndProtocol] = portBindings
	}

	ic.Env = make(map[string]string)
	for _, envVar := range dci.Config.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 1 {
			ic.Env[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			ic.Env[parts[0]] = ""
		}
	}

	ic.Args = append(ic.Args, dci.Config.Entrypoint...)
	ic.Args = append(ic.Args, dci.Config.Cmd...)

	for name, network := range dci.NetworkSettings.Networks {
		networkId := network.NetworkID
		if networkId == "" {
			networkId = name
		}
		ic.Networks = append(
			ic.Networks,
			containers.InspectedContainerNetwork{
				Id:         networkId,
				Name:       name,
				IPAddress:  network.IPAddress,
				Gateway:    network.Gateway,
				MacAddress: network.MacAddress,
			},
		)
	}

	ic.Labels = dci.Config.Labels

	return nil
}

func unmarshalNetwork(data []byte, net *containers.InspectedNetwork) error {
	if data == nil {
		return fmt.Errorf("the Docker command timed out without returning network data")
	}

	var dcn dockerInspectedNetwork
	err := json.Unmarshal(data, &dcn)
	if err != nil {
		return err
	}

	net.Id = dcn.Id
	net.Name = dcn.Name
	net.CreatedAt = dcn.Created
	net.Driver = dcn.Driver
	net.Scope = dcn.Scope
	net.Labels = dcn.Labels
	net.Attachable = dcn.Attachable
	net.Internal = dcn.Internal
	net.Ingress = dcn.Ingress
	for i := range dcn.Ipam.Config {
		net.Subnets = append(net.Subnets, dcn.Ipam.Config[i].Subnet)
		net.Gateways = append(net.Gateways, dcn.Ipam.Config[i].Gateway)
	}
	for id := range dcn.Containers {
		net.Containers = append(net.Containers, containers.InspectedNetworkContainer{
			Id:   id,
			Name: dcn.Containers[id].Name,
		})
	}

	return nil
}

type dockerInfo struct {
	DockerRootDir string `json:"DockerRootDir,omitempty"`
}

// dockerInspectedImageXxx correspond to data returned by "docker image inspect" command.
// The definition only includes data that we care about.
// For reference see https://github.com/moby/moby/blob/master/api/swagger.yaml
type dockerInspectedImage struct {
	Id     string                     `json:"Id"`
	Config dockerInspectedImageConfig `json:"Config,omitempty"`
}

type dockerInspectedImageConfig struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

// dockerInspectedContainerXxx correspond to data returned by "docker container inspect" command.
// The definition only includes data that we care about.
// For reference see https://github.com/moby/moby/blob/master/api/swagger.yaml
type dockerInspectedContainer struct {
	Id              string                                  `json:"Id"`
	Name            string                                  `json:"Name,omitempty"`
	Mounts          []dockerInspectedContainerMount         `json:"Mounts,omitempty"`
	Config          dockerInspectedContainerConfig          `json:"Config,omitempty"`
	Created         time.Time                               `json:"Created,omitempty"`
	State           dockerInspectedContainerState           `json:"State,omitempty"`
	NetworkSettings dockerInspectedContainerNetworkSettings `json:"NetworkSettings,omitempty"`
}

type dockerInspectedContainerMount struct {
	Type        apiv1.VolumeMountType `json:"Type,omitempty"`
	Name        string                `json:"Name,omitempty"`
	Source      string                `json:"Source,omitempty"`
	Destination string                `json:"Destination,omitempty"`
	ReadWrite   bool                  `json:"RW,omitempty"`
}

type dockerInspectedContainerConfig struct {
	Image      string            `json:"Image,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type dockerInspectedContainerState struct {
	Status     containers.ContainerStatus `json:"Status,omitempty"`
	StartedAt  time.Time                  `json:"StartedAt,omitempty"`
	FinishedAt time.Time                  `json:"FinishedAt,omitempty"`
	ExitCode   int32                      `json:"ExitCode,omitempty"`
	Error      string                     `json:"Error,omitempty"`
}

type dockerInspectedContainerNetworkSettings struct {
	Ports    containers.InspectedContainerPortMapping                  `json:"Ports,omitempty"`
	Networks map[string]dockerInspectedContainerNetworkSettingsNetwork `json:"Networks,omitempty"`
}

type dockerInspectedContainerNetworkSettingsNetwork struct {
	NetworkID  string `json:"NetworkID"`
	IPAddress  string `json:"IPAddress,omitempty"`
	Gateway    string `json:"Gateway,omitempty"`
	MacAddress string `json:"MacAddress,omitempty"`
}

type dockerInspectedNetwork struct {
	Id         string                                     `json:"Id"`
	Name       string                                     `json:"Name"`
	Created    time.Time                                  `json:"Created"`
	Scope      string                                     `json:"Scope"`
	Driver     string                                     `json:"Driver"`
	EnableIPv6 bool                                       `json:"EnableIPv6"`
	Internal   bool                                       `json:"Internal"`
	Attachable bool                                       `json:"Attachable"`
	Ingress    bool                                       `json:"Ingress"`
	Ipam       dockerInspectedNetworkIpam                 `json:"IPAM"`
	Labels     map[string]string                          `json:"Labels"`
	Containers map[string]dockerInspectedNetworkContainer `json:"Containers"`
}

type dockerInspectedNetworkContainer struct {
	Name string `json:"Name"`
}

type dockerInspectedNetworkIpam struct {
	Config []dockerInspectedNetworkIpamConfig `json:"Config"`
}

type dockerInspectedNetworkIpamConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

// Docker is using a nonstandard time format for network creation time,
// so we need to use a custom type to unmarshal it correctly.
type DockerListedNetworkTimestamp time.Time

const dockerListedNetworkTimestampLayout = "2006-01-02 15:04:05.999999999 -0700 UTC"

func (dlt DockerListedNetworkTimestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(dlt).Format(dockerListedNetworkTimestampLayout))
}

func (dlt *DockerListedNetworkTimestamp) UnmarshalJSON(b []byte) error {
	// GO json library leaves the quotes surrounding the value as part of the input to this method, so we need to remove them... :-/
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("invalid timestamp format: %s", string(b))
	}
	b = b[1 : len(b)-1]

	t, err := time.Parse(dockerListedNetworkTimestampLayout, string(b))
	if err != nil {
		return err
	}
	*dlt = DockerListedNetworkTimestamp(t)
	return nil
}

type dockerListedNetwork struct {
	CreatedAt DockerListedNetworkTimestamp `json:"CreatedAt,omitempty"`
	Driver    string                       `json:"Driver,omitempty"`
	ID        string                       `json:"ID"`
	IPv6      string                       `json:"IPv6,omitempty"`
	Internal  string                       `json:"Internal,omitempty"`
	Labels    string                       `json:"Labels,omitempty"`
	Name      string                       `json:"Name"`
}

func unmarshalListedNetwork(data []byte, net *containers.ListedNetwork) error {
	if data == nil {
		return fmt.Errorf("the Docker command timed out without returning network data")
	}

	var dln dockerListedNetwork
	err := json.Unmarshal(data, &dln)
	if err != nil {
		return err
	}

	net.Created = time.Time(dln.CreatedAt)
	net.Driver = dln.Driver
	net.ID = dln.ID
	if dln.IPv6 == "true" {
		net.IPv6 = true
	} else {
		net.IPv6 = false
	}
	if dln.Internal == "true" {
		net.Internal = true
	} else {
		net.Internal = false
	}

	labels := make(map[string]string)
	labelStrs := strings.Split(dln.Labels, ",")
	for _, labelStr := range labelStrs {
		if len(labelStr) < 3 {
			continue // Expect "key=value"
		}
		kv := strings.SplitN(labelStr, "=", 2)
		if len(kv) == 2 && len(kv[0]) > 0 && len(kv[1]) > 0 {
			labels[kv[0]] = kv[1]
		}
	}
	net.Labels = labels

	net.Name = dln.Name

	return nil
}

func normalizeCliErrors(originalErr error, errBuf *bytes.Buffer, errorMatches ...containers.ErrorMatch) error {
	errorMatches = append(errorMatches, newDockerNotRunningError)
	return containers.NormalizeCliError(originalErr, errBuf, errorMatches...)
}

var _ containers.VolumeOrchestrator = (*DockerCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*DockerCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*DockerCliOrchestrator)(nil)
