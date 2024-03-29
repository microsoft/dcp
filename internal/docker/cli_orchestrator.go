package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

var (
	volumeNotFoundRegEx       = regexp.MustCompile(`(?i)no such volume`)
	networkNotFoundRegEx      = regexp.MustCompile(`(?i)network (.*) not found`)
	containerNotFoundRegEx    = regexp.MustCompile(`(?i)no such container`)
	networkAlreadyExistsRegEx = regexp.MustCompile(`(?i)network with name (.*) already exists`)

	newContainerNotFoundErrorMatch    = containers.NewCliErrorMatch(containerNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
	newVolumeNotFoundErrorMatch       = containers.NewCliErrorMatch(volumeNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
	newNetworkNotFoundErrorMatch      = containers.NewCliErrorMatch(networkNotFoundRegEx, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
	newNetworkAlreadyExistsErrorMatch = containers.NewCliErrorMatch(networkAlreadyExistsRegEx, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("network already exists")))

	// We expect almost all Docker CLI invocations to finish within this time.
	// Telemetry shows there is a very long tail for Docker command completion times, so we use a conservative default.
	ordinaryDockerCommandTimeout = 30 * time.Second
)

type DockerCliOrchestrator struct {
	log logr.Logger

	// Process executor for running Docker commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *containers.EventWatcher

	// Event watcher for network events
	networkEvtWatcher *containers.EventWatcher
}

func NewDockerCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	dco := &DockerCliOrchestrator{
		log:      log,
		executor: executor,
	}

	dco.containerEvtWatcher = containers.NewEventWatcher(dco, dco.doWatchContainers, log)
	dco.networkEvtWatcher = containers.NewEventWatcher(dco, dco.doWatchNetworks, log)

	return dco
}

func (*DockerCliOrchestrator) ContainerHost() string {
	return "host.docker.internal"
}

func (dco *DockerCliOrchestrator) CheckStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	// Check the status of the Docker runtime
	statusCh := make(chan containers.ContainerRuntimeStatus, 1)
	go func() {
		// Run a simple command to check if the Docker CLI is installed and responsive
		cmd := makeDockerCommand("container", "ls", "-l")
		_, stdErr, err := dco.runDockerCommand(ctx, "Status", cmd, ordinaryDockerCommandTimeout)

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
		cmd := makeDockerCommand("info", "--format", "{{json .}}")
		outBuf, _, err := dco.runDockerCommand(ctx, "AliasDetection", cmd, ordinaryDockerCommandTimeout)
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

	status := <-statusCh
	isDocker := <-isDockerCh

	if status.Installed && status.Running && !isDocker {
		status.Installed = false
		status.Running = false
		status.Error = "docker command appears to be aliased to a different container runtime"
	}

	return status
}

func (dco *DockerCliOrchestrator) CreateVolume(ctx context.Context, name string) error {
	cmd := makeDockerCommand("volume", "create", name)
	outBuf, _, err := dco.runDockerCommand(ctx, "CreateVolume", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
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

	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectVolumes", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)))
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
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveVolumes", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, volumes)
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
			portVal = fmt.Sprintf("127.0.0.1:%s", portVal)
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

	if options.RestartPolicy != "" && options.RestartPolicy != apiv1.RestartPolicyNone {
		args = append(args, fmt.Sprintf("--restart=%s", options.RestartPolicy))
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
	outBuf, _, err := dco.runDockerCommand(ctx, "CreateContainer", cmd, 10*time.Minute)
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
	outBuf, _, err := dco.runDockerCommand(ctx, "RunContainer", cmd, 10*time.Minute)
	if err != nil {
		return "", err
	}
	return asId(outBuf)
}

func (dco *DockerCliOrchestrator) InspectContainers(ctx context.Context, names []string) ([]containers.InspectedContainer, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	cmd := makeDockerCommand(append(
		[]string{"container", "inspect", "--format", "{{json .}}"},
		names...)...,
	)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	return asObjects(outBuf, unmarshalContainer)
}

func (dco *DockerCliOrchestrator) StartContainers(ctx context.Context, containerIDs []string) ([]string, error) {
	if len(containerIDs) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, containerIDs...)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "StartContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(containerIDs)))
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
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "StopContainers", cmd, timeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
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
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	return dco.containerEvtWatcher.Watch(sink)
}

func (dco *DockerCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout io.WriteCloser, stderr io.WriteCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"container", "logs"}
	args = options.Apply(args)
	args = append(args, container)

	cmd := makeDockerCommand(args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	exitHandler := func(_ process.Pid_t, exitCode int32, err error) {
		if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
			dco.log.Error(stdOutCloseErr, "closing stdout log destination failed", "Container", container)
		}
		if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
			dco.log.Error(stdErrCloseErr, "closing stderr log destination failed", "Container", container)
		}

		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				dco.log.Error(err, "capturing container logs failed", "Container", container)
			}
		} else if exitCode != 0 && exitCode != process.UnknownExitCode {
			dco.log.Error(
				fmt.Errorf("capturing container logs failed with exit code %d", exitCode),
				"capturing container logs failed",
				"Container", container,
			)
		}
	}
	_, startWaitForProcessExit, err := dco.executor.StartProcess(ctx, cmd, process.ProcessExitHandlerFunc(exitHandler))
	if err != nil {
		return err
	}
	startWaitForProcessExit()
	return nil
}

func (dco *DockerCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	return dco.networkEvtWatcher.Watch(sink)
}

func (dco *DockerCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	args := []string{"network", "create"}

	if options.IPv6 {
		args = append(args, "--ipv6")
	}

	args = append(args, options.Name)

	cmd := makeDockerCommand(args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "CreateNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return "", containers.NormalizeCliError(err, errBuf, newNetworkAlreadyExistsErrorMatch.MaxObjects(1))
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
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveNetworks", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
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
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectNetworks", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
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
	_, errBuf, err := dco.runDockerCommand(ctx, "ConnectNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
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
	_, errBuf, err := dco.runDockerCommand(ctx, "DisconnectNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
	}
	return nil
}

func (dco *DockerCliOrchestrator) doWatchContainers(watcherCtx context.Context) {
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
				dco.containerEvtWatcher.Notify(evtMessage)
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
	pid, startWaitForProcessExit, err := dco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		dco.log.Error(err, "could not execute 'docker events' command; container events unavailable")
		return
	}

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

func (dco *DockerCliOrchestrator) doWatchNetworks(watcherCtx context.Context) {
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

			evtData := scanner.Text()
			var evtMessage containers.EventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				dco.log.Error(unmarshalErr, "network event data could not be parsed", "EventData", evtData)
			} else {
				dco.networkEvtWatcher.Notify(evtMessage)
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
	pid, startWaitForProcessExit, err := dco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		dco.log.Error(err, "could not execute 'docker events' command; network events unavailable")
		return
	}

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

func (dco *DockerCliOrchestrator) runDockerCommand(ctx context.Context, commandName string, cmd *exec.Cmd, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dco.log.V(1).Info("Running Docker command", "Command", cmd.String())
	exitCode, err := process.RunWithTimeout(effectiveCtx, dco.executor, cmd)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// If a timeout occurs, the content of the stdout and stderr buffers is not guaranteed to be complete.
		return nil, nil, err
	}

	// process.RunWithTimeout() guarantees (through exec.Cmd standard library implementation)
	// that when it exits without timeout error, all the available data written to stdout and stderr has been captured.

	stderr := ""
	stdout := ""
	if err != nil || exitCode != 0 {
		if errBuf.Len() > 0 {
			stderr = errBuf.String()
		}
		if outBuf.Len() > 0 {
			stdout = outBuf.String()
		}
	}
	if err != nil {
		return outBuf, errBuf, fmt.Errorf("%s command failed: %w Stdout: '%s' Stderr: '%s'", commandName, err, stdout, stderr)
	}
	if exitCode != 0 {
		return outBuf, errBuf, fmt.Errorf("%s command returned error code %d Stdout: '%s' Stderr: '%s'", commandName, exitCode, stdout, stderr)
	}

	return outBuf, errBuf, nil
}

func makeDockerCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	return cmd
}

// Docker CLI returns JSON lines for most output, so split the lines before unmarshalling.
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
	ic.Ports = dci.NetworkSettings.Ports
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
		net.ContainerIDs = append(net.ContainerIDs, id)
	}

	return nil
}

type dockerInfo struct {
	DockerRootDir string `json:"DockerRootDir,omitempty"`
}

// dockerInspectedContainerXxx correspond to data returned by "docker container inspect" command.
// The definition only includes data that we care about.
// For reference see https://github.com/moby/moby/blob/master/api/swagger.yaml
type dockerInspectedContainer struct {
	Id              string                                  `json:"Id"`
	Name            string                                  `json:"Name,omitempty"`
	Config          dockerInspectedContainerConfig          `json:"Config,omitempty"`
	Created         time.Time                               `json:"Created,omitempty"`
	State           dockerInspectedContainerState           `json:"State,omitempty"`
	NetworkSettings dockerInspectedContainerNetworkSettings `json:"NetworkSettings,omitempty"`
}

type dockerInspectedContainerConfig struct {
	Image      string   `json:"Image,omitempty"`
	Env        []string `json:"Env,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	Entrypoint []string `json:"Entrypoint,omitempty"`
}

type dockerInspectedContainerState struct {
	Status     containers.ContainerStatus `json:"Status,omitempty"`
	StartedAt  time.Time                  `json:"StartedAt,omitempty"`
	FinishedAt time.Time                  `json:"FinishedAt,omitempty"`
	ExitCode   int32                      `json:"ExitCode,omitempty"`
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
	Id         string                     `json:"Id"`
	Name       string                     `json:"Name"`
	Created    time.Time                  `json:"Created"`
	Scope      string                     `json:"Scope"`
	Driver     string                     `json:"Driver"`
	EnableIPv6 bool                       `json:"EnableIPv6"`
	Internal   bool                       `json:"Internal"`
	Attachable bool                       `json:"Attachable"`
	Ingress    bool                       `json:"Ingress"`
	Ipam       dockerInspectedNetworkIpam `json:"IPAM"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]struct{}        `json:"Containers"`
}

type dockerInspectedNetworkIpam struct {
	Config []dockerInspectedNetworkIpamConfig `json:"Config"`
}

type dockerInspectedNetworkIpamConfig struct {
	Subnet  string `json:"Subnet,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

var _ containers.VolumeOrchestrator = (*DockerCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*DockerCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*DockerCliOrchestrator)(nil)
