package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"github.com/go-logr/logr"

	v1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

var (
	LF                        = []byte("\n")
	volumeNotFoundRegEx       = regexp.MustCompile(`(?i)no such volume`)
	networkNotFoundRegEx      = regexp.MustCompile(`(?i)network (.*) not found`)
	containerNotFoundRegEx    = regexp.MustCompile(`(?i)no such container`)
	networkAlreadyExistsRegEx = regexp.MustCompile(`(?i)network with name (.*) already exists`)

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

func NewDockerCliOrchestrator(log logr.Logger, executor process.Executor) *DockerCliOrchestrator {
	dco := &DockerCliOrchestrator{
		log:      log,
		executor: executor,
	}

	dco.containerEvtWatcher = containers.NewEventWatcher(dco, dco.doWatchContainers, log)
	dco.networkEvtWatcher = containers.NewEventWatcher(dco, dco.doWatchNetworks, log)

	return dco
}

func (dco *DockerCliOrchestrator) CreateVolume(ctx context.Context, name string) error {
	cmd := makeDockerCommand(ctx, "volume", "create", name)
	outBuf, _, err := dco.runDockerCommand(ctx, "CreateVolume", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return err
	}
	return expectStrings(outBuf, []string{name})
}

func (dco *DockerCliOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	if len(volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	cmd := makeDockerCommand(ctx, append(
		[]string{"volume", "inspect", "--format", "{{json .}}"},
		volumes...)...,
	)

	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectVolumes", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newVolumeNotFoundErrorMatch(len(volumes)))
	}

	return asObjects(outBuf, unmarshalVolume)
}

func (dco *DockerCliOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, volumes...)
	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveVolumes", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newVolumeNotFoundErrorMatch(len(volumes)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, expectStrings(outBuf, volumes)
}

func applyCreateContainerOptions(args []string, options v1.ContainerSpec) []string {
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

	if options.RestartPolicy != "" && options.RestartPolicy != v1.RestartPolicyNone {
		args = append(args, fmt.Sprintf("--restart=%s", options.RestartPolicy))
	}

	if options.Command != "" {
		args = append(args, "--entrypoint", options.Command)
	}

	return args
}

func (dco *DockerCliOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	args := []string{"create"}

	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

	args = applyCreateContainerOptions(args, options.ContainerSpec)

	args = append(args, options.Image)

	if (options.Args != nil) && (len(options.Args) > 0) {
		args = append(args, options.Args...)
	}

	cmd := makeDockerCommand(ctx, args...)

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

	args = applyCreateContainerOptions(args, options.ContainerSpec)

	args = append(args, "--detach")
	args = append(args, options.Image)

	if (options.Args != nil) && (len(options.Args) > 0) {
		args = append(args, options.Args...)
	}

	cmd := makeDockerCommand(ctx, args...)

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

	cmd := makeDockerCommand(ctx, append(
		[]string{"container", "inspect", "--format", "{{json .}}"},
		names...)...,
	)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newContainerNotFoundErrorMatch(len(names)))
	}

	return asObjects(outBuf, unmarshalContainer)
}

func (dco *DockerCliOrchestrator) StartContainers(ctx context.Context, containers []string) ([]string, error) {
	if len(containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "start"}
	args = append(args, containers...)

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "StartContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newContainerNotFoundErrorMatch(len(containers)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	started := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return started, expectStrings(outBuf, containers)
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

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "StopContainers", cmd, timeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newContainerNotFoundErrorMatch(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	stopped := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return stopped, expectStrings(outBuf, names)
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

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newContainerNotFoundErrorMatch(len(names)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, expectStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	return dco.containerEvtWatcher.Watch(sink)
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

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "CreateNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return "", normalizeError(err, errBuf, newNetworkAlreadyExistsErrorMatch(1))
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

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveNetworks", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newNetworkNotFoundErrorMatch(len(options.Networks)))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, expectStrings(outBuf, options.Networks)
}

func (dco *DockerCliOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	if len(options.Networks) == 0 {
		return nil, fmt.Errorf("must specify at least one network")
	}

	args := []string{"network", "inspect", "--format", "{{json .}}"}
	args = append(args, options.Networks...)

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "InspectNetworks", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, newNetworkNotFoundErrorMatch(len(options.Networks)))
	}

	return asObjects(outBuf, unmarshalNetwork)
}

func (dco *DockerCliOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	args := []string{"network", "connect"}

	for i := range options.Aliases {
		args = append(args, "--alias", options.Aliases[i])
	}

	args = append(args, options.Network, options.Container)

	cmd := makeDockerCommand(ctx, args...)
	_, errBuf, err := dco.runDockerCommand(ctx, "ConnectNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return normalizeError(err, errBuf, newContainerNotFoundErrorMatch(1), newNetworkNotFoundErrorMatch(1))
	}
	return nil
}

func (dco *DockerCliOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	args := []string{"network", "disconnect"}

	if options.Force {
		args = append(args, "--force")
	}

	args = append(args, options.Network, options.Container)

	cmd := makeDockerCommand(ctx, args...)
	_, errBuf, err := dco.runDockerCommand(ctx, "DisconnectNetwork", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return normalizeError(err, errBuf, newContainerNotFoundErrorMatch(1), newNetworkNotFoundErrorMatch(1))
	}
	return nil
}

func (dco *DockerCliOrchestrator) doWatchContainers(watcherCtx context.Context) {
	args := []string{"events", "--filter", "type=container", "--format", "{{json .}}"}
	cmd := makeDockerCommand(watcherCtx, args...)

	reader, writer := io.NewBufferedPipe()
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
		err := dco.executor.StopProcess(pid)
		if err != nil {
			dco.log.Error(err, "could not stop 'docker events' command")
		}
	}
}

func (dco *DockerCliOrchestrator) doWatchNetworks(watcherCtx context.Context) {
	args := []string{"events", "--filter", "type=network", "--format", "{{json .}}"}
	cmd := makeDockerCommand(watcherCtx, args...)

	reader, writer := io.NewBufferedPipe()
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
		err := dco.executor.StopProcess(pid)
		if err != nil {
			dco.log.Error(err, "could not stop 'docker events' command")
		}
	}
}

func (dco *DockerCliOrchestrator) runDockerCommand(ctx context.Context, commandName string, cmd *exec.Cmd, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

type errorMatch struct {
	regex              *regexp.Regexp
	err                error
	maxObjectsAffected int
}

func newErrorMatch(regex *regexp.Regexp, maxObjectsAffected int, err ...error) errorMatch {
	realErr := containers.ErrNotFound
	if len(err) > 0 {
		realErr = err[0]
	}
	return errorMatch{
		regex:              regex,
		err:                realErr,
		maxObjectsAffected: maxObjectsAffected,
	}
}

func newContainerNotFoundErrorMatch(maxObjectsAffected int) errorMatch {
	return newErrorMatch(containerNotFoundRegEx, maxObjectsAffected, errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")))
}

func newVolumeNotFoundErrorMatch(maxObjectsAffected int) errorMatch {
	return newErrorMatch(volumeNotFoundRegEx, maxObjectsAffected, errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")))
}

func newNetworkNotFoundErrorMatch(maxObjectsAffected int) errorMatch {
	return newErrorMatch(networkNotFoundRegEx, maxObjectsAffected, errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")))
}

func newNetworkAlreadyExistsErrorMatch(maxObjectsAffected int) errorMatch {
	return newErrorMatch(networkAlreadyExistsRegEx, maxObjectsAffected, errors.Join(containers.ErrAlreadyExists, fmt.Errorf("network already exists")))
}

func normalizeError(originalError error, errBuf *bytes.Buffer, errorMatches ...errorMatch) error {
	if originalError == nil {
		return nil
	}

	if errBuf == nil {
		return originalError
	}

	if len(errorMatches) == 0 {
		return originalError
	}

	lines := bytes.Split(errBuf.Bytes(), []byte("\n"))

	for i := range errorMatches {
		allMatch := slices.All(lines, func(l []byte) bool {
			line := bytes.TrimSpace(l)
			return len(line) == 0 || errorMatches[i].regex.Match(line)
		})

		// We might have some empty lines, hence #lines >= #volumes
		if allMatch && len(lines) >= errorMatches[i].maxObjectsAffected {
			// All errors were of "not found" type--safe to report ErrNotFound
			return errorMatches[i].err
		}
	}

	return originalError
}

func makeDockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd
}

func expectStrings(b *bytes.Buffer, expected []string) error {
	if len(expected) == 0 {
		return fmt.Errorf("need to provide at least one expected string") // Should never happen
	}
	if slices.LenIf(expected, func(s string) bool { return len(s) == 0 }) > 0 {
		return fmt.Errorf("expected strings must not be empty") // Also should never happen
	}

	notFoundErr := func() error {
		return fmt.Errorf("'%s' not found in output ('%s')", expected, b.String())
	}

	chunks := bytes.Split(b.Bytes(), LF)
	i := 0

	for _, chunk := range chunks {
		chunk = bytes.TrimSpace(chunk)
		if len(chunk) == 0 {
			continue
		}

		if string(chunk) != expected[i] {
			return notFoundErr()
		}

		i++
		if i == len(expected) {
			return nil // We found all the strings we wanted
		}
	}

	// We run out of chunks before finding all expected strings
	return notFoundErr()
}

func asObjects[T any](b *bytes.Buffer, unmarshalFn func([]byte, *T) error) ([]T, error) {
	if b == nil {
		return nil, fmt.Errorf("the Docker command timed out without returning any data")
	}

	retval := []T{}
	chunks := bytes.Split(b.Bytes(), LF)

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

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), LF), bytes.TrimSpace))
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
	Image string `json:"Image,omitempty"`
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
