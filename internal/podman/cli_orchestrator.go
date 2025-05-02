package podman

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
	"runtime"
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
	volumeNotFoundRegEx           = regexp.MustCompile(`(?i)no such volume`)
	networkNotFoundRegEx          = regexp.MustCompile(`(?i)network (.*) not found`)
	containerNotFoundRegEx        = regexp.MustCompile(`(?i)no such container`)
	networkAlreadyExistsRegEx     = regexp.MustCompile(`(?i)network with name (.*) already exists`)
	containerAlreadyAttachedRegEx = regexp.MustCompile(`(?i)container (.*) is already connected to network`)
	unableToConnectRegEx          = regexp.MustCompile(`(?i)unable to connect to Podman socket:`)
	volumeInUseRegEx              = regexp.MustCompile(`(?i)volume is being used`)
	volumeAlreadyExistsRegEx      = regexp.MustCompile(`(?i)volume already exists`)

	newContainerNotFoundErrorMatch        = containers.NewCliErrorMatch(containerNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
	newVolumeNotFoundErrorMatch           = containers.NewCliErrorMatch(volumeNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
	volumeInUseErrorMatch                 = containers.NewCliErrorMatch(volumeInUseRegEx, errors.Join(containers.ErrObjectInUse, fmt.Errorf("volume is being used by a container")))
	volumeAlreadyExistsErrorMatch         = containers.NewCliErrorMatch(volumeAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("volume already exists")))
	newNetworkNotFoundErrorMatch          = containers.NewCliErrorMatch(networkNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
	newNetworkAlreadyExistsErrorMatch     = containers.NewCliErrorMatch(networkAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("network already exists")))
	newContainerAlreadyAttachedErrorMatch = containers.NewCliErrorMatch(containerAlreadyAttachedRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("container already attached")))
	newPodmanNotRunningErrorMatch         = containers.NewCliErrorMatch(unableToConnectRegEx, errors.Join(containers.ErrRuntimeNotHealthy, fmt.Errorf("podman runtime is not healthy")))

	// We expect almost all Podman CLI invocations to finish within this time.
	// Telemetry shows there is a very long tail for Podman command completion times, so we use a conservative default.
	ordinaryPodmanCommandTimeout = 30 * time.Second

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

type PodmanCliOrchestrator struct {
	log logr.Logger

	// Process executor for running Podman commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]

	// Event watcher for network events
	networkEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]
}

func NewPodmanCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	pco := &PodmanCliOrchestrator{
		log:      log,
		executor: executor,
	}

	pco.containerEvtWatcher = pubsub.NewSubscriptionSet(pco.doWatchContainers, context.Background())
	pco.networkEvtWatcher = pubsub.NewSubscriptionSet(pco.doWatchNetworks, context.Background())

	return pco
}

func (*PodmanCliOrchestrator) IsDefault() bool {
	return false
}

func (*PodmanCliOrchestrator) Name() string {
	return "podman"
}

func (*PodmanCliOrchestrator) ContainerHost() string {
	return "host.containers.internal"
}

func (pco *PodmanCliOrchestrator) CheckStatus(ctx context.Context, cacheUsage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
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
			// Timed out, assume Podman is not responsive and unavailable
			return containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     "Timed out while checking Podman status; Podman CLI is not responsive.",
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

	newStatus := pco.getStatus(ctx)
	updateStatus.Lock()
	// Update the cached status
	cachedStatus = &newStatus
	updateStatus.Unlock()

	return newStatus
}

// Check the status of the Podman runtime in the background until the context is canceled.
func (pco *PodmanCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if !backgroundStatusUpdates.CompareAndSwap(0, 1) {
		return
	}

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		for {
			// Only one goroutine should be checking the status at a time
			if checkStatusSyncCh.TryLock() {
				newStatus := pco.getStatus(ctx)

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

func (pco *PodmanCliOrchestrator) getStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	cmd := makePodmanCommand("container", "ls", "--last", "1", "--quiet")
	_, stdErr, err := pco.runBufferedPodmanCommand(ctx, "Info", cmd, nil, nil, ordinaryPodmanCommandTimeout)

	if errors.Is(err, exec.ErrNotFound) {
		// Try to get the inner error if this is an exec.ErrNotFound error
		if unwrapErr := errors.Unwrap(err); errors.Is(unwrapErr, exec.ErrNotFound) {
			err = unwrapErr
		}

		// Couldn't find the Podman CLI, so it's not installed
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

		// Error response from the Podman command, assume runtime isn't available
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

func (pco *PodmanCliOrchestrator) CreateVolume(ctx context.Context, name string) error {
	cmd := makePodmanCommand("volume", "create", name)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "CreateVolume", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf, volumeAlreadyExistsErrorMatch.MaxObjects(1))
	}
	return containers.ExpectCliStrings(outBuf, []string{name})
}

func (pco *PodmanCliOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	if len(volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	cmd := makePodmanCommand(append(
		[]string{"volume", "inspect", "--format", "json"},
		volumes...)...,
	)

	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "InspectVolumes", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)))
	}

	return asObjects(outBuf, unmarshalVolume)
}

func (pco *PodmanCliOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, volumes...)
	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "RemoveVolumes", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)), volumeInUseErrorMatch.MaxObjects(len(volumes)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, volumes)
}

func (pco *PodmanCliOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
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

	// Append the build context argument
	args = append(args, options.Context)

	cmd := makePodmanCommand(args...)

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
	_, _, err := pco.runBufferedPodmanCommand(ctx, "BuildImage", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return err
	}

	return nil
}

func (pco *PodmanCliOrchestrator) InspectImages(ctx context.Context, images []string) ([]containers.InspectedImage, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}

	cmd := makePodmanCommand(append(
		[]string{"image", "inspect", "--format", "{{json .}}"},
		images...)...,
	)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "InspectImages", cmd, nil, nil, ordinaryPodmanCommandTimeout)
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

func (pco *PodmanCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
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

	cmd := makePodmanCommand(args...)

	// Create container can take a long time to finish if the image is not available locally.
	// Use a much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultCreateContainerTimeout
	}

	outBuf, _, err := pco.runBufferedPodmanCommand(ctx, "CreateContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		if id, err2 := asId(outBuf); err2 == nil {
			// We got an ID, so the container was created, but the command failed.
			return id, err
		}

		return "", err
	}

	return asId(outBuf)
}

func (pco *PodmanCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
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

	cmd := makePodmanCommand(args...)

	// The run container command can take a long time to finish if the image is not available locally.
	// So we use much longer timeout than for other commands.
	if options.Timeout == 0 {
		options.Timeout = defaultRunContainerTimeout
	}

	outBuf, _, err := pco.runBufferedPodmanCommand(ctx, "RunContainer", cmd, options.StdOutStream, options.StdErrStream, options.Timeout)
	if err != nil {
		return "", err
	}
	return asId(outBuf)
}

func (pco *PodmanCliOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
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

	cmd := makePodmanCommand(args...)

	cmd.Stdout = options.StdOutStream
	cmd.Stderr = options.StdErrStream

	exitCh := make(chan int32)
	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		// We only care about the exit code, not the error. The only scenario where we should get an error
		// is if the context for an exec command is canceled during DCP shutdown, in which case that's expected.
		if !errors.Is(err, context.Canceled) {
			pco.log.Error(err, "unexpected error during container exec command", "Command", cmd.String())
		}
		exitCh <- exitCode
		close(exitCh)
	}

	pco.log.V(1).Info("Running Podman command", "Command", cmd.String())
	_, _, startWaitForProcessExit, err := pco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler))
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start Podman command '%s'", "ExecContainer"))
	}
	startWaitForProcessExit()

	return exitCh, nil
}

func (pco *PodmanCliOrchestrator) InspectContainers(ctx context.Context, names []string) ([]containers.InspectedContainer, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	cmd := makePodmanCommand(append(
		[]string{"container", "inspect", "--format", "json"},
		names...)...,
	)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "InspectContainers", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	return asObjects(outBuf, unmarshalContainer)
}

func (pco *PodmanCliOrchestrator) StartContainers(ctx context.Context, containerIds []string, streamOptions containers.StreamCommandOptions) ([]string, error) {
	if len(containerIds) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, containerIds...)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "StartContainers", cmd, streamOptions.StdOutStream, streamOptions.StdErrStream, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(containerIds)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	started := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return started, containers.ExpectCliStrings(outBuf, containerIds)
}

func (pco *PodmanCliOrchestrator) StopContainers(ctx context.Context, names []string, secondsToKill uint) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "stop"}
	var timeout time.Duration = ordinaryPodmanCommandTimeout
	if secondsToKill > 0 {
		args = append(args, "--time", fmt.Sprintf("%d", secondsToKill))
		timeout = time.Duration(secondsToKill)*time.Second + ordinaryPodmanCommandTimeout
	}
	args = append(args, names...)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "StopContainers", cmd, nil, nil, timeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	stopped := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return stopped, containers.ExpectCliStrings(outBuf, names)
}

func (pco *PodmanCliOrchestrator) RemoveContainers(ctx context.Context, names []string, force bool) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "rm", "-v"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, names...)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "RemoveContainers", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, names)
}

func (pco *PodmanCliOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	args := []string{"container", "cp"}

	// Read the tar file to copy from standard input
	// Apply ownership information from the tar file
	args = append(args, "-a=false", "-")

	tarWriter := usvc_io.NewTarWriter()

	for _, item := range options.Entries {
		if item.Type == apiv1.FileSystemEntryTypeDir {
			if addDirectoryErr := containers.AddDirectoryToTar(tarWriter, ".", options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, pco.log); addDirectoryErr != nil {
				return addDirectoryErr
			}
		} else {
			if addFileErr := containers.AddFileToTar(tarWriter, ".", options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, pco.log); addFileErr != nil {
				return addFileErr
			}
		}
	}

	buffer, bufferErr := tarWriter.Buffer()
	if bufferErr != nil {
		return bufferErr
	}

	args = append(args, options.Container+":"+options.Destination)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// TODO: Remove this workaround once podman cli supports copy via stdio on Windows
		cmd = makePodmanMachineCommand(args...)
	} else {
		cmd = makePodmanCommand(args...)
	}

	cmd.Stdin = buffer

	_, errBuf, err := pco.runBufferedPodmanCommand(ctx, "CopyFile", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf)
	}

	return nil
}

func (pco *PodmanCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := pco.containerEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (pco *PodmanCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout usvc_io.WriteSyncerCloser, stderr usvc_io.WriteSyncerCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"container", "logs"}
	args = options.Apply(args)
	args = append(args, container)

	cmd := makePodmanCommand(args...)

	exitCh, err := pco.streamPodmanCommand(ctx, "CaptureContainerLogs", cmd, stdout, stderr, streamCommandOptionUseWatcher)
	if err != nil {
		return err
	}

	go func() {
		// Wait for the command to finish and clean up any resources
		exitErr := <-exitCh
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) && !errors.Is(exitErr, context.DeadlineExceeded) {
			pco.log.Error(err, "capturing container logs failed", "Container", container)
		}

		if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
			pco.log.Error(stdOutCloseErr, "closing stdout log destination failed", "Container", container)
		}
		if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
			pco.log.Error(stdErrCloseErr, "closing stderr log destination failed", "Container", container)
		}
	}()

	return nil
}

func (pco *PodmanCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	sub := pco.networkEvtWatcher.Subscribe(sink)
	return sub, nil
}

func (pco *PodmanCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	args := []string{"network", "create"}

	if options.IPv6 {
		args = append(args, "--ipv6")
	}

	for key, value := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", key, value))
	}

	args = append(args, options.Name)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "CreateNetwork", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return "", normalizeCliErrors(err, errBuf, newNetworkAlreadyExistsErrorMatch.MaxObjects(1))
	}
	return asId(outBuf)
}

func (pco *PodmanCliOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "rm"}
	if options.Force {
		args = append(args, "--force")
	}
	args = append(args, options.Networks...)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "RemoveNetworks", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, options.Networks)
}

func (pco *PodmanCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "inspect", "--format", "json"}
	args = append(args, options.Networks...)

	cmd := makePodmanCommand(args...)
	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "InspectNetworks", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
	}

	return asObjects(outBuf, unmarshalNetwork)
}

func (pco *PodmanCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	args := []string{"network", "connect"}

	for i := range options.Aliases {
		args = append(args, "--alias", options.Aliases[i])
	}

	args = append(args, options.Network, options.Container)

	cmd := makePodmanCommand(args...)
	_, errBuf, err := pco.runBufferedPodmanCommand(ctx, "ConnectNetwork", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1), newContainerAlreadyAttachedErrorMatch)
	}
	return nil
}

func (pco *PodmanCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	args := []string{"network", "disconnect"}

	if options.Force {
		args = append(args, "--force")
	}

	args = append(args, options.Network, options.Container)

	cmd := makePodmanCommand(args...)
	_, errBuf, err := pco.runBufferedPodmanCommand(ctx, "DisconnectNetwork", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return normalizeCliErrors(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
	}
	return nil
}

func (pco *PodmanCliOrchestrator) ListNetworks(ctx context.Context) ([]containers.ListedNetwork, error) {
	args := []string{"network", "ls", "--format", "json"}

	cmd := makePodmanCommand(args...)

	outBuf, errBuf, err := pco.runBufferedPodmanCommand(ctx, "ListNetworks", cmd, nil, nil, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(err, errBuf)
	}

	return asObjects(outBuf, unmarshalListedNetwork)
}

func (pco *PodmanCliOrchestrator) DefaultNetworkName() string {
	return "podman"
}

func (pco *PodmanCliOrchestrator) doWatchContainers(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	args := []string{"events", "--filter", "type=container", "--format", "json"}
	cmd := makePodmanCommand(args...)

	reader, writer := usvc_io.NewBufferedPipe()
	cmd.Stdout = writer
	defer writer.Close() // Ensure that the following scanner goroutine ends.

	scanner := bufio.NewScanner(reader)
	scanner.Buffer([]byte{}, 2048*1024) // Default max Scanner buffer is only 65k, which might not be enough for Podman events

	go func() {
		for scanner.Scan() {
			if watcherCtx.Err() != nil {
				return // Cancellation has been requested, so we should stop scanning events
			}

			var evtMessage podmanEventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				pco.log.Error(unmarshalErr, "container event data could not be parsed", "EventData", scanner.Text())
			} else {
				ss.Notify((&evtMessage).ToEventMessage())
			}
		}

		if scanner.Err() != nil {
			pco.log.Error(scanner.Err(), "scanning for container events resulted in an error")
		}
	}()

	pic := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pic)

	// Container events are delivered on best-effort basis.
	// If the "podman events" command fails unexpectedly, we will log the error,
	// but we won't try to restart it.
	pid, startTime, startWaitForProcessExit, err := pco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		pco.log.Error(err, "could not execute 'podman events' command; container events unavailable")
		return
	}

	dcpproc.RunWatcher(pco.executor, pid, startTime, pco.log)

	startWaitForProcessExit()

	var exitInfo process.ProcessExitInfo
	select {
	case exitInfo = <-pic:
		if exitInfo.Err != nil {
			pco.log.Error(err, "'podman events' command failed")
		}
	case <-watcherCtx.Done():
		// We are asked to shut down
		pco.log.V(1).Info("stopping 'podman events' command", "pid", pid)
	}
}

func (pco *PodmanCliOrchestrator) doWatchNetworks(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	args := []string{"events", "--filter", "type=network", "--format", "json"}
	cmd := makePodmanCommand(args...)

	reader, writer := usvc_io.NewBufferedPipe()
	cmd.Stdout = writer
	defer writer.Close() // Ensure that the following scanner goroutine ends.

	scanner := bufio.NewScanner(reader)
	scanner.Buffer([]byte{}, 2048*1024) // Default max Scanner buffer is only 65k, which might not be enough for Podman events

	go func() {
		for scanner.Scan() {
			if watcherCtx.Err() != nil {
				return // Cancellation has been requested, so we should stop scanning events
			}

			evtData := scanner.Text()
			var evtMessage containers.EventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				pco.log.Error(unmarshalErr, "network event data could not be parsed", "EventData", evtData)
			} else {
				ss.Notify(evtMessage)
			}
		}

		if scanner.Err() != nil {
			pco.log.Error(scanner.Err(), "scanning for network events resulted in an error")
		}
	}()

	pic := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pic)

	// Container events are delivered on best-effort basis.
	// If the "podman events" command fails unexpectedly, we will log the error,
	// but we won't try to restart it.
	pid, startTime, startWaitForProcessExit, err := pco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		pco.log.Error(err, "could not execute 'podman events' command; network events unavailable")
		return
	}

	dcpproc.RunWatcher(pco.executor, pid, startTime, pco.log)

	startWaitForProcessExit()

	var exitInfo process.ProcessExitInfo
	select {
	case exitInfo = <-pic:
		if exitInfo.Err != nil {
			pco.log.Error(err, "'podman events' command failed")
		}
	case <-watcherCtx.Done():
		// We are asked to shut down
		pco.log.V(1).Info("stopping 'podman events' command", "pid", pid)
	}
}

type streamPodmanCommandOption uint32

const (
	streamCommandOptionNone       streamPodmanCommandOption = 0
	streamCommandOptionUseWatcher streamPodmanCommandOption = 1
)

func (pco *PodmanCliOrchestrator) streamPodmanCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdOutWriter io.Writer,
	stdErrWriter io.Writer,
	opts streamPodmanCommandOption,
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
			exitCh <- fmt.Errorf("podman command '%s' returned with non-zero exit code %d", commandName, exitCode)
		}
	}

	pco.log.V(1).Info("running podman command", "Command", cmd.String())
	pid, startTime, startWaitForProcessExit, err := pco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler))
	if err != nil {
		close(exitCh)
		return nil, errors.Join(err, fmt.Errorf("failed to start podman command '%s'", commandName))
	}

	if opts&streamCommandOptionUseWatcher != 0 {
		dcpproc.RunWatcher(pco.executor, pid, startTime, pco.log)
	}

	startWaitForProcessExit()

	return exitCh, nil
}

func (pco *PodmanCliOrchestrator) runBufferedPodmanCommand(
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

	exitCh, err := pco.streamPodmanCommand(effectiveCtx, commandName, cmd, stdOutWriter, stdErrWriter, streamCommandOptionNone)
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

func makePodmanCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("podman", args...)
	return cmd
}

func makePodmanMachineCommand(args ...string) *exec.Cmd {
	args = append([]string{"machine", "ssh", "podman"}, args...)
	cmd := exec.Command("podman", args...)
	return cmd
}

// Podman CLI returns a JSON array for most of its commands, so unmarshal the entire output as an array of values.
func asObjects[T any, V any](b *bytes.Buffer, unmarshalFn func(*V, *T) error) ([]T, error) {
	if b == nil {
		return nil, fmt.Errorf("the Podman command timed out without returning any data")
	}

	retval := []T{}

	var unmarshalRaw []V
	err := json.Unmarshal(b.Bytes(), &unmarshalRaw)
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
		return "", fmt.Errorf("the Podman command timed out without returning object identifier")
	}

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), osutil.LF()), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

func unmarshalVolume(pvi *podmanInspectedVolume, vol *containers.InspectedVolume) error {
	vol.Name = pvi.Name
	vol.Driver = pvi.Driver
	vol.MountPoint = pvi.MountPoint
	vol.Scope = pvi.Scope
	vol.CreatedAt = pvi.CreatedAt
	vol.Labels = pvi.Labels

	return nil
}

func unmarshalImage(pii *podmanInspectedImage, ic *containers.InspectedImage) error {
	ic.Id = pii.Id
	ic.Labels = pii.Config.Labels

	return nil
}

func unmarshalContainer(pci *podmanInspectedContainer, ic *containers.InspectedContainer) error {
	ic.Id = pci.Id
	ic.Name = pci.Name
	ic.Image = pci.Config.Image
	ic.CreatedAt = pci.Created
	ic.StartedAt = pci.State.StartedAt
	ic.FinishedAt = pci.State.FinishedAt
	ic.Status = pci.State.Status
	ic.ExitCode = pci.State.ExitCode
	ic.Error = pci.State.Error

	ic.Mounts = make([]apiv1.VolumeMount, len(pci.Mounts))
	for i, mount := range pci.Mounts {
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
	for portAndProtocol, portBindings := range pci.NetworkSettings.Ports {
		if len(portAndProtocol) == 0 || len(portBindings) == 0 {
			continue // Skip ports that are published but not mapped to host.
		}
		ic.Ports[portAndProtocol] = portBindings
	}

	ic.Env = make(map[string]string)
	for _, envVar := range pci.Config.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 1 {
			ic.Env[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			ic.Env[parts[0]] = ""
		}
	}

	ic.Args = append(ic.Args, pci.Config.Entrypoint...)
	ic.Args = append(ic.Args, pci.Config.Cmd...)

	for name, network := range pci.NetworkSettings.Networks {
		ic.Networks = append(
			ic.Networks,
			containers.InspectedContainerNetwork{
				Id:         network.NetworkID,
				Name:       name,
				IPAddress:  network.IPAddress,
				Gateway:    network.Gateway,
				MacAddress: network.MacAddress,
			},
		)
	}

	ic.Labels = pci.Config.Labels

	return nil
}

func unmarshalNetwork(pcn *podmanInspectedNetwork, net *containers.InspectedNetwork) error {
	net.Id = pcn.Id
	net.Name = pcn.Name
	net.CreatedAt = pcn.Created
	net.Driver = pcn.Driver
	net.Scope = "local"
	net.Labels = pcn.Labels
	net.Attachable = true
	net.Internal = pcn.Internal
	net.Ingress = false
	for i := range pcn.Subnets {
		net.Subnets = append(net.Subnets, pcn.Subnets[i].Subnet)
		net.Gateways = append(net.Gateways, pcn.Subnets[i].Gateway)
	}
	for id := range pcn.Containers {
		net.Containers = append(net.Containers, containers.InspectedNetworkContainer{
			Id:   id,
			Name: pcn.Containers[id].Name,
		})
	}

	return nil
}

type podmanInspectedVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	MountPoint string            `json:"Mountpoint"`
	CreatedAt  time.Time         `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels,omitempty"`
	Scope      string            `json:"Scope"`
}

// podmanInspectedImageXxx correspond to data returned by "podman image inspect" command.
// The definition only includes data that we care about.
type podmanInspectedImage struct {
	Id     string                     `json:"Id"`
	Config podmanInspectedImageConfig `json:"Config,omitempty"`
}

type podmanInspectedImageConfig struct {
	Labels map[string]string `json:"Labels,omitempty"`
}

// podmanInspectedContainerXxx correspond to data returned by "podman container inspect" command.
// The definition only includes data that we care about.
type podmanInspectedContainer struct {
	Id              string                                  `json:"Id"`
	Name            string                                  `json:"Name,omitempty"`
	Config          podmanInspectedContainerConfig          `json:"Config,omitempty"`
	Created         time.Time                               `json:"Created,omitempty"`
	State           podmanInspectedContainerState           `json:"State,omitempty"`
	Mounts          []podmanInspectedContainerMount         `json:"Mounts,omitempty"`
	NetworkSettings podmanInspectedContainerNetworkSettings `json:"NetworkSettings,omitempty"`
}

// Custom type to handle the fact that podman 4.x returns entrypoint as a string, while
// podman 5.x returns it as an array of strings.
type podmanEntrypoint []string

func (pe *podmanEntrypoint) UnmarshalJSON(b []byte) error {
	var maybeArray []string
	arrayErr := json.Unmarshal(b, &maybeArray)
	if arrayErr != nil {
		var maybeString string
		stringErr := json.Unmarshal(b, &maybeString)
		if stringErr != nil {
			return fmt.Errorf("error parsing container inspect: Entrypoint is neither a string nor an array of strings")
		}

		// entrypoint was a string, normalize to an array of one value
		*pe = podmanEntrypoint{maybeString}
		return nil
	}

	// entrypoint is an array of strings
	*pe = podmanEntrypoint(maybeArray)
	return nil
}

type podmanInspectedContainerConfig struct {
	Image      string            `json:"Image,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Entrypoint podmanEntrypoint  `json:"Entrypoint,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type podmanInspectedContainerState struct {
	Status     containers.ContainerStatus `json:"Status,omitempty"`
	StartedAt  time.Time                  `json:"StartedAt,omitempty"`
	FinishedAt time.Time                  `json:"FinishedAt,omitempty"`
	ExitCode   int32                      `json:"ExitCode,omitempty"`
	Error      string                     `json:"Error,omitempty"`
}

type podmanInspectedContainerMount struct {
	Type        apiv1.VolumeMountType `json:"Type,omitempty"`
	Name        string                `json:"Name,omitempty"`
	Source      string                `json:"Source,omitempty"`
	Destination string                `json:"Destination,omitempty"`
	ReadWrite   bool                  `json:"RW,omitempty"`
}

type podmanInspectedContainerNetworkSettings struct {
	Ports    containers.InspectedContainerPortMapping                  `json:"Ports,omitempty"`
	Networks map[string]podmanInspectedContainerNetworkSettingsNetwork `json:"Networks,omitempty"`
}

type podmanInspectedContainerNetworkSettingsNetwork struct {
	NetworkID  string `json:"NetworkID"`
	IPAddress  string `json:"IPAddress,omitempty"`
	Gateway    string `json:"Gateway,omitempty"`
	MacAddress string `json:"MacAddress,omitempty"`
}

type podmanInspectedNetwork struct {
	Id         string                                     `json:"id"`
	Name       string                                     `json:"name"`
	Created    time.Time                                  `json:"created"`
	Driver     string                                     `json:"driver"`
	EnableIPv6 bool                                       `json:"ipv6_enabled"`
	Internal   bool                                       `json:"internal"`
	DnsEnabled bool                                       `json:"dns_enabled"`
	Subnets    []podmanInspectedNetworkSubnet             `json:"subnets"`
	Labels     map[string]string                          `json:"labels,omitempty"`
	Containers map[string]podmanInspectedNetworkContainer `json:"Containers"`
}

type podmanInspectedNetworkContainer struct {
	Name string `json:"name"`
}

type podmanInspectedNetworkSubnet struct {
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

type podmanListedNetwork struct {
	Name     string            `json:"name"`
	Id       string            `json:"id"`
	Driver   string            `json:"driver,omitempty"`
	Created  time.Time         `json:"created,omitempty"`
	IPv6     bool              `json:"ipv6_enabled,omitempty"`
	Internal bool              `json:"internal,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

func unmarshalListedNetwork(pln *podmanListedNetwork, net *containers.ListedNetwork) error {
	net.Name = pln.Name
	net.ID = pln.Id
	net.Driver = pln.Driver
	net.Created = pln.Created
	net.IPv6 = pln.IPv6
	net.Internal = pln.Internal
	net.Labels = pln.Labels

	return nil
}

type podmanEventMessage struct {
	// The type of the object that caused the event
	Source containers.EventSource `json:"Type"`

	// The ID of the resource triggering the event
	ID string `json:"ID,omitempty"`

	// The status change that triggered the event
	Action containers.EventAction `json:"Status"`
}

func (pem *podmanEventMessage) ToEventMessage() containers.EventMessage {
	if pem.Source == containers.EventSourceNetwork {
		// Podman only returns network events for containers, not the actual networks
		return containers.EventMessage{
			Source: pem.Source,
			Action: pem.Action,
			Actor: containers.EventActor{
				ID: pem.ID,
			},
			Attributes: map[string]string{
				"container": pem.ID,
			},
		}
	}

	return containers.EventMessage{
		Source: pem.Source,
		Action: pem.Action,
		Actor: containers.EventActor{
			ID: pem.ID,
		},
		Attributes: make(map[string]string),
	}
}

func normalizeCliErrors(originalErr error, errBuf *bytes.Buffer, errorMatches ...containers.ErrorMatch) error {
	errorMatches = append(errorMatches, newPodmanNotRunningErrorMatch)
	return containers.NormalizeCliError(originalErr, errBuf, errorMatches...)
}

var _ containers.VolumeOrchestrator = (*PodmanCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*PodmanCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*PodmanCliOrchestrator)(nil)
