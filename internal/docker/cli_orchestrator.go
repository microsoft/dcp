package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/go-logr/logr"

	v1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

var (
	LF                     = []byte("\n")
	volumeNotFoundRegEx    = regexp.MustCompile(`(?i)no such volume`)
	containerNotFoundRegEx = regexp.MustCompile(`(?i)no such container`)

	// We expect almost all Docker CLI invocations to finish within this time.
	// Telemetry shows there is a very long tail for Docker command completion times, so we use a conservative default.
	ordinaryDockerCommandTimeout = 30 * time.Second
)

type DockerCliOrchestrator struct {
	log logr.Logger

	// Process executor for running Docker commands
	executor process.Executor

	// Map storing container event subscriptions
	evtSubscriptions *syncmap.Map[handleT, *dockerEventSubscription]

	// Cancellation function for container event watcher goroutine
	// nil if there are no container event subscriptions
	evtWatcherCancelFunc context.CancelFunc

	// Count of event subscriptions. When that count goes to zero,
	// the container event watcher goroutine gets cancelled.
	evtSubscriptionCount uint32

	// A lock that we take when starting/stopping the container event watcher goroutine
	evtWatcherLock *sync.Mutex
}

func NewDockerCliOrchestrator(log logr.Logger, executor process.Executor) *DockerCliOrchestrator {
	return &DockerCliOrchestrator{
		log:                  log,
		executor:             executor,
		evtSubscriptions:     &syncmap.Map[handleT, *dockerEventSubscription]{},
		evtWatcherCancelFunc: nil,
		evtWatcherLock:       &sync.Mutex{},
		evtSubscriptionCount: 0,
	}
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
		return nil, normalizeError(err, errBuf, volumeNotFoundRegEx, len(volumes))
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
		return nil, normalizeError(err, errBuf, volumeNotFoundRegEx, len(volumes))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, expectStrings(outBuf, volumes)
}

func (dco *DockerCliOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	args := []string{"run"}

	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}

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

	args = append(args, "--detach")
	args = append(args, options.Image)
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
		return nil, normalizeError(err, errBuf, containerNotFoundRegEx, len(names))
	}

	return asObjects(outBuf, unmarshalContainer)
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
		return nil, normalizeError(err, errBuf, containerNotFoundRegEx, len(names))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	stopped := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return stopped, expectStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) RemoveContainers(ctx context.Context, names []string, force bool) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	args := []string{"container", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, names...)

	cmd := makeDockerCommand(ctx, args...)
	outBuf, errBuf, err := dco.runDockerCommand(ctx, "RemoveContainers", cmd, ordinaryDockerCommandTimeout)
	if err != nil {
		return nil, normalizeError(err, errBuf, containerNotFoundRegEx, len(names))
	}

	nonEmpty := slices.NonEmpty[byte](bytes.Split(outBuf.Bytes(), LF))
	removed := slices.Map[[]byte, string](nonEmpty, func(bs []byte) string { return string(bs) })
	return removed, expectStrings(outBuf, names)
}

func (dco *DockerCliOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (containers.EventSubscription, error) {
	sub := newDockerEventSubscription(dco, sink)
	dco.evtSubscriptions.Store(sub.handle, sub)

	dco.evtWatcherLock.Lock()
	defer dco.evtWatcherLock.Unlock()
	dco.evtSubscriptionCount++

	if dco.evtSubscriptionCount == 1 {
		watcherCtx, cancelFunc := context.WithCancel(context.Background())
		dco.evtWatcherCancelFunc = cancelFunc
		go dco.doWatchContainers(watcherCtx)
	}

	return sub, nil
}

func (dco *DockerCliOrchestrator) eventSubscriptionCancelled(handle handleT) {
	dco.evtSubscriptions.Delete(handle)

	dco.evtWatcherLock.Lock()
	defer dco.evtWatcherLock.Unlock()

	if dco.evtSubscriptionCount == 0 {
		dco.log.Error(fmt.Errorf("eventSubscriptionCancelled called when there are no subscriptions"), "")
		return
	}
	dco.evtSubscriptionCount--
	if dco.evtSubscriptionCount == 0 {
		dco.evtWatcherCancelFunc()
		dco.evtWatcherCancelFunc = nil
	}
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
			evtData := scanner.Text()
			var evtMessage containers.EventMessage
			unmarshalErr := json.Unmarshal(scanner.Bytes(), &evtMessage)
			if unmarshalErr != nil {
				dco.log.Error(unmarshalErr, "container event data could not be parsed", "EventData", evtData)
			} else {
				dco.evtSubscriptions.Range(func(_ handleT, sub *dockerEventSubscription) bool {
					sub.notify(evtMessage)
					return true
				})
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

func (dco *DockerCliOrchestrator) runDockerCommand(ctx context.Context, commandName string, cmd *exec.Cmd, timeout time.Duration) (*bytes.Buffer, *bytes.Buffer, error) {
	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, err := process.Run(effectiveCtx, dco.executor, cmd)

	// process.Run() guarantees (through exec.Cmd standard library implementation)
	// that when it exits, all the available data written to stdout and stderr are captured.

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

func normalizeError(originalError error, errBuf *bytes.Buffer, isNotFound *regexp.Regexp, maxObjectsAffected int) error {
	if originalError == nil {
		return nil
	}

	lines := bytes.Split(errBuf.Bytes(), []byte("\n"))
	allIndicateNotFound := slices.All(lines, func(l []byte) bool {
		line := bytes.TrimSpace(l)
		return len(line) == 0 || isNotFound.Match(line)
	})

	// We might have some empty lines, hence #lines >= #volumes
	if allIndicateNotFound && len(lines) >= maxObjectsAffected {
		// All errors were of "not found" type--safe to report ErrNotFound
		return containers.ErrNotFound
	} else {
		// Some errors were different than "not found" kind--let's report the original error.
		return originalError
	}
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
	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), LF), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

func unmarshalVolume(data []byte, vol *containers.InspectedVolume) error {
	return json.Unmarshal(data, vol)
}

func unmarshalContainer(data []byte, ic *containers.InspectedContainer) error {
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
	Ports containers.InspectedContainerPortMapping `json:"Ports,omitempty"`
}

var _ containers.VolumeOrchestrator = (*DockerCliOrchestrator)(nil)
var _ containers.ContainerOrchestrator = (*DockerCliOrchestrator)(nil)
