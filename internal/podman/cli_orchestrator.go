package podman

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

	// We expect almost all Podman CLI invocations to finish within this time.
	// Telemetry shows there is a very long tail for Podman command completion times, so we use a conservative default.
	ordinaryPodmanCommandTimeout = 30 * time.Second
)

type PodmanCliOrchestrator struct {
	log logr.Logger

	// Process executor for running Podman commands
	executor process.Executor

	// Event watcher for container events
	containerEvtWatcher *containers.EventWatcher

	// Event watcher for network events
	networkEvtWatcher *containers.EventWatcher
}

func NewPodmanCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	pco := &PodmanCliOrchestrator{
		log:      log,
		executor: executor,
	}

	pco.containerEvtWatcher = containers.NewEventWatcher(pco, pco.doWatchContainers, log)
	pco.networkEvtWatcher = containers.NewEventWatcher(pco, pco.doWatchNetworks, log)

	return pco
}

func (*PodmanCliOrchestrator) ContainerHost() string {
	return "host.containers.internal"
}

func (pco *PodmanCliOrchestrator) CheckStatus(ctx context.Context) containers.ContainerRuntimeStatus {
	cmd := makePodmanCommand(ctx, "container", "ls", "--last", "1", "--quiet")
	_, stdErr, err := pco.runPodmanCommand(ctx, "Info", cmd, ordinaryPodmanCommandTimeout)

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
	cmd := makePodmanCommand(ctx, "volume", "create", name)
	outBuf, _, err := pco.runPodmanCommand(ctx, "CreateVolume", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return err
	}
	return containers.ExpectCliStrings(outBuf, []string{name})
}

func (pco *PodmanCliOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	if len(volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	cmd := makePodmanCommand(ctx, append(
		[]string{"volume", "inspect", "--format", "json"},
		volumes...)...,
	)

	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "InspectVolumes", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newVolumeNotFoundErrorMatch.MaxObjects(len(volumes)))
	}

	return asObjects(outBuf, unmarshalVolume)
}

func (pco *PodmanCliOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, volumes...)
	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "RemoveVolumes", cmd, ordinaryPodmanCommandTimeout)
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

	cmd := makePodmanCommand(ctx, args...)

	// Create container can take a long time to finish if the image is not available locally.
	// Use a much longer timeout than for other commands.
	outBuf, _, err := pco.runPodmanCommand(ctx, "CreateContainer", cmd, 10*time.Minute)
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

	cmd := makePodmanCommand(ctx, args...)

	// The run container command can take a long time to finish if the image is not available locally.
	// So we use much longer timeout than for other commands.
	outBuf, _, err := pco.runPodmanCommand(ctx, "RunContainer", cmd, 10*time.Minute)
	if err != nil {
		return "", err
	}
	return asId(outBuf)
}

func (pco *PodmanCliOrchestrator) InspectContainers(ctx context.Context, names []string) ([]containers.InspectedContainer, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	cmd := makePodmanCommand(ctx, append(
		[]string{"container", "inspect", "--format", "json"},
		names...)...,
	)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "InspectContainers", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	return asObjects(outBuf, unmarshalContainer)
}

func (pco *PodmanCliOrchestrator) StartContainers(ctx context.Context, containerIds []string) ([]string, error) {
	if len(containerIds) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, containerIds...)

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "StartContainers", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(containerIds)))
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

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "StopContainers", cmd, timeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
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

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "RemoveContainers", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), osutil.LF()))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, containers.ExpectCliStrings(outBuf, names)
}

func (pco *PodmanCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	return pco.containerEvtWatcher.Watch(sink)
}

func (dco *PodmanCliOrchestrator) CaptureContainerLogs(ctx context.Context, container string, stdout io.WriteCloser, stderr io.WriteCloser, options containers.StreamContainerLogsOptions) error {
	args := []string{"container", "logs"}
	args = options.Apply(args)
	args = append(args, container)

	cmd := makePodmanCommand(ctx, args...)
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
				fmt.Errorf("streaming container logs failed with exit code %d", exitCode),
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

func (pco *PodmanCliOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	return pco.networkEvtWatcher.Watch(sink)
}

func (pco *PodmanCliOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	args := []string{"network", "create"}

	if options.IPv6 {
		args = append(args, "--ipv6")
	}

	args = append(args, options.Name)

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "CreateNetwork", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return "", containers.NormalizeCliError(err, errBuf, newNetworkAlreadyExistsErrorMatch.MaxObjects(1))
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

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "RemoveNetworks", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
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

	cmd := makePodmanCommand(ctx, args...)
	outBuf, errBuf, err := pco.runPodmanCommand(ctx, "InspectNetworks", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return nil, containers.NormalizeCliError(err, errBuf, newNetworkNotFoundErrorMatch.MaxObjects(len(options.Networks)))
	}

	return asObjects(outBuf, unmarshalNetwork)
}

func (pco *PodmanCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	args := []string{"network", "connect"}

	for i := range options.Aliases {
		args = append(args, "--alias", options.Aliases[i])
	}

	args = append(args, options.Network, options.Container)

	cmd := makePodmanCommand(ctx, args...)
	_, errBuf, err := pco.runPodmanCommand(ctx, "ConnectNetwork", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
	}
	return nil
}

func (pco *PodmanCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	args := []string{"network", "disconnect"}

	if options.Force {
		args = append(args, "--force")
	}

	args = append(args, options.Network, options.Container)

	cmd := makePodmanCommand(ctx, args...)
	_, errBuf, err := pco.runPodmanCommand(ctx, "DisconnectNetwork", cmd, ordinaryPodmanCommandTimeout)
	if err != nil {
		return containers.NormalizeCliError(err, errBuf, newContainerNotFoundErrorMatch.MaxObjects(1), newNetworkNotFoundErrorMatch.MaxObjects(1))
	}
	return nil
}

func (pco *PodmanCliOrchestrator) doWatchContainers(watcherCtx context.Context) {
	args := []string{"events", "--filter", "type=container", "--format", "json"}
	cmd := makePodmanCommand(watcherCtx, args...)

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
			var evtMessage podmanEventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				pco.log.Error(unmarshalErr, "container event data could not be parsed", "EventData", evtData)
			} else {
				pco.containerEvtWatcher.Notify((&evtMessage).ToEventMessage())
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
	pid, startWaitForProcessExit, err := pco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		pco.log.Error(err, "could not execute 'podman events' command; container events unavailable")
		return
	}

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

func (pco *PodmanCliOrchestrator) doWatchNetworks(watcherCtx context.Context) {
	args := []string{"events", "--filter", "type=network", "--format", "json"}
	cmd := makePodmanCommand(watcherCtx, args...)

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
				pco.networkEvtWatcher.Notify(evtMessage)
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
	pid, startWaitForProcessExit, err := pco.executor.StartProcess(watcherCtx, cmd, peh)
	if err != nil {
		pco.log.Error(err, "could not execute 'podman events' command; network events unavailable")
		return
	}

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

func (pco *PodmanCliOrchestrator) runPodmanCommand(ctx context.Context, commandName string, cmd *exec.Cmd, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pco.log.V(1).Info("Running Podman command", "Command", cmd.String())
	exitCode, err := process.RunWithTimeout(effectiveCtx, pco.executor, cmd)
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

func makePodmanCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "podman", args...)
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

func unmarshalContainer(pci *podmanInspectedContainer, ic *containers.InspectedContainer) error {
	ic.Id = pci.Id
	ic.Name = pci.Name
	ic.Image = pci.Config.Image
	ic.CreatedAt = pci.Created
	ic.StartedAt = pci.State.StartedAt
	ic.FinishedAt = pci.State.FinishedAt
	ic.Status = pci.State.Status
	ic.ExitCode = pci.State.ExitCode
	ic.Ports = pci.NetworkSettings.Ports
	ic.Env = make(map[string]string)
	for _, envVar := range pci.Config.Env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 1 {
			ic.Env[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			ic.Env[parts[0]] = ""
		}
	}
	ic.Args = append(ic.Args, pci.Config.Entrypoint)
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
		net.ContainerIDs = append(net.ContainerIDs, id)
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

// podmanInspectedContainerXxx correspond to data returned by "podman container inspect" command.
// The definition only includes data that we care about.
type podmanInspectedContainer struct {
	Id              string                                  `json:"Id"`
	Name            string                                  `json:"Name,omitempty"`
	Config          podmanInspectedContainerConfig          `json:"Config,omitempty"`
	Created         time.Time                               `json:"Created,omitempty"`
	State           podmanInspectedContainerState           `json:"State,omitempty"`
	NetworkSettings podmanInspectedContainerNetworkSettings `json:"NetworkSettings,omitempty"`
}

type podmanInspectedContainerConfig struct {
	Image      string   `json:"Image,omitempty"`
	Env        []string `json:"Env,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	Entrypoint string   `json:"Entrypoint,omitempty"`
}

type podmanInspectedContainerState struct {
	Status     containers.ContainerStatus `json:"Status,omitempty"`
	StartedAt  time.Time                  `json:"StartedAt,omitempty"`
	FinishedAt time.Time                  `json:"FinishedAt,omitempty"`
	ExitCode   int32                      `json:"ExitCode,omitempty"`
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
	Id         string                         `json:"id"`
	Name       string                         `json:"name"`
	Created    time.Time                      `json:"created"`
	Driver     string                         `json:"driver"`
	EnableIPv6 bool                           `json:"ipv6_enabled"`
	Internal   bool                           `json:"internal"`
	DnsEnabled bool                           `json:"dns_enabled"`
	Subnets    []podmanInspectedNetworkSubnet `json:"subnets"`
	Labels     map[string]string              `json:"labels,omitempty"`
	Containers map[string]struct{}            `json:"Containers"`
}

type podmanInspectedNetworkSubnet struct {
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
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

var _ containers.VolumeOrchestrator = (*PodmanCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*PodmanCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*PodmanCliOrchestrator)(nil)
