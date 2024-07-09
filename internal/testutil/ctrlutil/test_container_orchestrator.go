package ctrlutil

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/secrets"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

var (
	// Base32 encoder used to generate unique postfixes for Executable replicas.
	randomNameEncoder = base32.HexEncoding.WithPadding(base32.NoPadding)
)

const (
	TestEventActionStopWithoutRemove containers.EventAction = "stop_without_remove"
)

type TestContainerOrchestrator struct {
	randomNameLength       int
	volumes                map[string]containerVolume
	networks               map[string]containerNetwork
	images                 map[string]bool
	imageIds               map[string]string
	imageSecrets           map[string]map[string]string
	containers             map[string]testContainer
	containersToFail       map[string]containerExit
	containerEventsWatcher *containers.EventWatcher
	networkEventsWatcher   *containers.EventWatcher
	mutex                  *sync.Mutex
	log                    logr.Logger
}

type containerExit struct {
	exitCode int32
	stdErr   string
}

func NewTestContainerOrchestrator(log logr.Logger) *TestContainerOrchestrator {
	to := &TestContainerOrchestrator{
		randomNameLength: 20,
		volumes:          map[string]containerVolume{},
		networks: map[string]containerNetwork{
			"bridge": {
				withId:    newId("bridge"),
				driver:    "bridge",
				isDefault: true,
			},
			"host": {
				withId:    newId("host"),
				driver:    "host",
				isDefault: true,
			},
			"none": {
				withId:    newId("none"),
				driver:    "none",
				isDefault: true,
			},
		},
		images:           map[string]bool{},
		imageIds:         map[string]string{},
		imageSecrets:     map[string]map[string]string{},
		containers:       map[string]testContainer{},
		containersToFail: map[string]containerExit{},
		mutex:            &sync.Mutex{},
		log:              log,
	}

	to.containerEventsWatcher = containers.NewEventWatcher(to, to.doWatchContainers, log)
	to.networkEventsWatcher = containers.NewEventWatcher(to, to.doWatchNetworks, log)

	return to
}

func (to *TestContainerOrchestrator) FailMatchingContainers(ctx context.Context, name string, exitCode int32, stdErr string) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	to.containersToFail[name] = containerExit{exitCode: exitCode, stdErr: stdErr}

	go func() {
		<-ctx.Done()
		to.mutex.Lock()
		defer to.mutex.Unlock()

		delete(to.containersToFail, name)
	}()
}

func (to *TestContainerOrchestrator) doWatchContainers(watcherCtx context.Context) {
}

func (to *TestContainerOrchestrator) doWatchNetworks(watcherCtx context.Context) {
}

func (to *TestContainerOrchestrator) getRandomName() (string, error) {
	postfixBytes := make([]byte, to.randomNameLength)
	if read, err := rand.Read(postfixBytes); err != nil {
		return "", err
	} else if read != to.randomNameLength {
		return "", fmt.Errorf("could not generate %d bytes of randomness", to.randomNameLength)
	}

	return randomNameEncoder.EncodeToString(postfixBytes), nil
}

type withId struct {
	id   string
	name string
}

func newId(name string) withId {
	return withId{id: getID(), name: name}
}

func (obj withId) matches(name string) bool {
	if obj.name == name {
		return true
	}

	if name != "" && strings.HasPrefix(obj.id, name) {
		return true
	}

	return false
}

type containerVolume struct {
	name    string
	created time.Time
}

type containerNetwork struct {
	withId
	driver     string
	ipv6       bool
	gateways   []string
	subnets    []string
	containers []string
	isDefault  bool
}

type testContainer struct {
	withId
	image      string
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time
	status     containers.ContainerStatus
	exitCode   int32
	ports      map[string][]containers.InspectedContainerHostPortConfig
	networks   []string
	args       []string
	env        map[string]string
}

func getID() string {
	return uuid.New().String()
}

func (*TestContainerOrchestrator) ContainerHost() string {
	return "host.test.internal"
}

func (to *TestContainerOrchestrator) CheckStatus(ctx context.Context, ignoreCache bool) containers.ContainerRuntimeStatus {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	return containers.ContainerRuntimeStatus{
		Installed: true,
		Running:   true,
	}
}

func (to *TestContainerOrchestrator) CreateVolume(ctx context.Context, name string) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if _, found := to.volumes[name]; found {
		return containers.ErrAlreadyExists
	}

	to.volumes[name] = containerVolume{name: name, created: time.Now().UTC()}

	return nil
}

func (to *TestContainerOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	for _, name := range volumes {
		if _, found := to.volumes[name]; !found {
			return nil, containers.ErrNotFound
		}
	}

	for _, name := range volumes {
		delete(to.volumes, name)
	}

	return volumes, nil
}

func (to *TestContainerOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var result []containers.InspectedVolume

	for _, name := range volumes {
		volume, found := to.volumes[name]
		if !found {
			return nil, containers.ErrNotFound
		}

		result = append(result, containers.InspectedVolume{
			Name:       name,
			Driver:     "local",
			MountPoint: "",
			Scope:      "local",
			Labels:     map[string]string{},
			CreatedAt:  volume.created,
		})
	}

	return result, nil
}

func (to *TestContainerOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	return to.networkEventsWatcher.Watch(sink)
}

func (to *TestContainerOrchestrator) CreateNetwork(ctx context.Context, options containers.CreateNetworkOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if options.Name == "" {
		return "", fmt.Errorf("name must not be empty")
	}

	if _, found := to.networks[options.Name]; found {
		return "", containers.ErrAlreadyExists
	}

	id := newId(options.Name)

	to.networks[options.Name] = containerNetwork{
		withId:     id,
		driver:     "bridge",
		ipv6:       options.IPv6,
		gateways:   []string{},
		subnets:    []string{},
		containers: []string{},
	}

	// Notify listeners that we've created the network
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceNetwork,
		Action: containers.EventActionCreate,
		Actor:  containers.EventActor{ID: id.id},
	})

	return id.id, nil
}

func (to *TestContainerOrchestrator) RemoveNetworks(ctx context.Context, options containers.RemoveNetworksOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	names := []string{}
	ids := []string{}
	for _, name := range options.Networks {
		var found bool
		for _, network := range to.networks {
			if network.matches(name) {
				if network.isDefault {
					return nil, fmt.Errorf("cannot remove default network")
				}

				if len(network.containers) > 0 {
					return nil, fmt.Errorf("cannot remove network with containers")
				}

				found = true
				names = append(names, network.name)
				ids = append(ids, network.id)
			}

			if !found && !options.Force {
				return nil, containers.ErrNotFound
			}
		}
	}

	for _, name := range names {
		id := to.networks[name].id
		delete(to.networks, name)

		// Notify listeners that we've destroyed the network
		to.containerEventsWatcher.Notify(containers.EventMessage{
			Source: containers.EventSourceNetwork,
			Action: containers.EventActionDestroy,
			Actor:  containers.EventActor{ID: id},
		})
	}

	return ids, nil
}

func (to *TestContainerOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var result []containers.InspectedNetwork

	for _, name := range options.Networks {
		var found bool
		for _, network := range to.networks {
			if network.matches(name) {
				found = true

				runningContainers := slices.Select(network.containers, func(containerId string) bool {
					return to.containers[containerId].status == containers.ContainerStatusRunning
				})

				result = append(result, containers.InspectedNetwork{
					Id:           network.id,
					Name:         network.name,
					Driver:       network.driver,
					IPv6:         network.ipv6,
					Gateways:     network.gateways,
					Subnets:      network.subnets,
					ContainerIDs: runningContainers,
					Labels:       map[string]string{},
					CreatedAt:    time.Now().UTC(),
				})
			}
		}

		if !found {
			return nil, containers.ErrNotFound
		}
	}

	return result, nil
}

func (to *TestContainerOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, network := range to.networks {
		if network.matches(options.Network) {
			for _, container := range to.containers {
				if container.matches(options.Container) {
					return to.doConnectNetwork(ctx, network, container)
				}
			}

			return errors.Join(containers.ErrNotFound, fmt.Errorf("container not found"))
		}
	}

	return errors.Join(containers.ErrNotFound, fmt.Errorf("network not found"))
}

func (to *TestContainerOrchestrator) doConnectNetwork(ctx context.Context, network containerNetwork, container testContainer) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !slices.Contains(network.containers, container.id) {
		network.containers = append(network.containers, container.id)
		container.networks = append(container.networks, network.name)

		to.containers[container.id] = container
		to.networks[network.name] = network

		// Notify listeners that we've connected the container to the network
		to.networkEventsWatcher.Notify(containers.EventMessage{
			Source:     containers.EventSourceNetwork,
			Action:     containers.EventActionConnect,
			Actor:      containers.EventActor{ID: network.id},
			Attributes: map[string]string{"container": container.id},
		})
	}

	return nil
}

func (to *TestContainerOrchestrator) DisconnectNetwork(ctx context.Context, options containers.DisconnectNetworkOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, network := range to.networks {
		if network.matches(options.Network) {
			for _, container := range to.containers {
				if container.matches(options.Container) {
					network.containers = slices.Select(network.containers, func(id string) bool {
						return container.id != id
					})
					container.networks = slices.Select(container.networks, func(name string) bool {
						return network.name != name
					})
					to.networks[network.name] = network
					to.containers[container.id] = container

					// Notify listeners that we've disconnected the container from the network
					to.networkEventsWatcher.Notify(containers.EventMessage{
						Source:     containers.EventSourceNetwork,
						Action:     containers.EventActionDisconnect,
						Actor:      containers.EventActor{ID: network.id},
						Attributes: map[string]string{"container": container.id},
					})

					return nil
				}
			}

			if !options.Force {
				return errors.Join(containers.ErrNotFound, fmt.Errorf("container not found"))
			}
		}
	}

	if !options.Force {
		return errors.Join(containers.ErrNotFound, fmt.Errorf("network not found"))
	}

	return nil
}

func (to *TestContainerOrchestrator) DefaultNetworkName() string {
	return "bridge"
}

func (to *TestContainerOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if options.IidFile != "" {
		guid := uuid.New().String()
		err := usvc_io.WriteFile(options.IidFile, []byte(guid), osutil.PermissionOwnerReadWriteOthersRead)
		if err != nil {
			return err
		}
		to.images[guid] = true

		for _, imageTag := range options.Tags {
			to.imageIds[imageTag] = guid
		}
	}

	for _, image := range options.Tags {
		to.images[image] = true

		to.imageSecrets[image] = map[string]string{}

		for _, secret := range options.Secrets {
			if secret.Type == apiv1.EnvSecret && secret.Value != "" {
				to.imageSecrets[image][secret.ID] = secrets.DecryptSecret(secret.Value)
			}
		}
	}

	return nil
}

func (to *TestContainerOrchestrator) GetImageId(tag string) (string, bool) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	id, found := to.imageIds[tag]
	return id, found
}

func (to *TestContainerOrchestrator) GetImageSecrets(tag string) (map[string]string, bool) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	secrets, found := to.imageSecrets[tag]
	return secrets, found
}

func (to *TestContainerOrchestrator) HasImage(tag string) bool {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	_, found := to.images[tag]
	return found
}

func (to *TestContainerOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*containers.EventSubscription, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	return to.containerEventsWatcher.Watch(sink)
}

func (to *TestContainerOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	container, err := to.doCreateContainer(ctx, options.Name, options.ContainerSpec)
	if err != nil {
		return "", err
	}

	if err = to.doConnectNetwork(ctx, to.networks["bridge"], container); err != nil {
		return container.id, err
	}

	return container.id, nil
}

func (to *TestContainerOrchestrator) doCreateContainer(ctx context.Context, name string, options apiv1.ContainerSpec) (testContainer, error) {
	if ctx.Err() != nil {
		return testContainer{}, ctx.Err()
	}

	if name != "" {
		for _, existing := range to.containers {
			if existing.name == name {
				return testContainer{}, containers.ErrAlreadyExists
			}
		}
	} else {
		if randomName, err := to.getRandomName(); err != nil {
			return testContainer{}, err
		} else {
			name = randomName
		}
	}

	id := newId(name)

	container := testContainer{
		withId:    id,
		image:     options.Image,
		createdAt: time.Now().UTC(),
		status:    containers.ContainerStatusCreated,
		networks:  []string{"bridge"},
		ports:     map[string][]containers.InspectedContainerHostPortConfig{},
		args:      options.Args,
		env:       map[string]string{},
	}

	for _, env := range options.Env {
		container.env[env.Name] = env.Value
	}

	for _, port := range options.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}

		hostPort := port.HostPort
		if port.HostPort == 0 {
			hostPort = port.ContainerPort
		}

		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}

		container.ports[fmt.Sprintf("%d/%s", port.ContainerPort, protocol)] = []containers.InspectedContainerHostPortConfig{
			{HostIp: hostIP, HostPort: fmt.Sprintf("%d", hostPort)},
		}
	}

	to.containers[id.id] = container

	// Notify listeners that we've created the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionCreate,
		Actor:  containers.EventActor{ID: id.id},
	})

	return container, nil
}

func (to *TestContainerOrchestrator) StartContainers(ctx context.Context, names []string, streamOptions containers.StreamCommandOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var result []string

	var containersToStart []testContainer

	for _, name := range names {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToStart = append(containersToStart, container)
				found = true
			}
		}

		if !found {
			return nil, containers.ErrNotFound
		}
	}

	for _, container := range containersToStart {
		id, err := to.doStartContainer(ctx, container, streamOptions)
		if err != nil {
			return result, err
		}

		result = append(result, id)
	}

	return result, nil
}

func (to *TestContainerOrchestrator) doStartContainer(ctx context.Context, container testContainer, streamOptions containers.StreamCommandOptions) (string, error) {
	streamIfPossible := func(err error) error {
		if streamOptions.StdErrStream != nil {
			_, writeErr := streamOptions.StdErrStream.Write([]byte(err.Error()))
			err = errors.Join(err, writeErr)
		}
		return err
	}

	if ctx.Err() != nil {
		return "", streamIfPossible(ctx.Err())
	}

	for name, exit := range to.containersToFail {
		if container.matches(name) || strings.HasPrefix(container.image, name) {
			container.status = containers.ContainerStatusExited
			container.startedAt = time.Now().UTC()
			container.finishedAt = time.Now().UTC()
			container.exitCode = exit.exitCode

			to.containers[container.id] = container

			to.containerEventsWatcher.Notify(containers.EventMessage{
				Source: containers.EventSourceContainer,
				Action: containers.EventActionDie,
				Actor:  containers.EventActor{ID: container.id},
			})

			return container.id, streamIfPossible(fmt.Errorf("container failed to start: %s", exit.stdErr))
		}
	}

	if container.status != containers.ContainerStatusCreated && container.status != containers.ContainerStatusExited {
		return "", streamIfPossible(fmt.Errorf("container is not in a state to be started"))
	}

	container.status = containers.ContainerStatusRunning
	container.startedAt = time.Now().UTC()

	to.containers[container.id] = container

	// Notify listeners that we've started the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionStart,
		Actor:  containers.EventActor{ID: container.id},
	})

	return container.id, nil
}

func (to *TestContainerOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	container, err := to.doCreateContainer(ctx, options.Name, options.ContainerSpec)
	if err != nil {
		return "", err
	}

	if _, err = to.doStartContainer(ctx, container, options.StreamCommandOptions); err != nil {
		return container.id, err
	}

	return container.id, nil
}

func (to *TestContainerOrchestrator) StopContainers(ctx context.Context, names []string, secondsToKill uint) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var containersToStop []testContainer
	for _, name := range names {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToStop = append(containersToStop, container)
				found = true
			}
		}

		if !found {
			return nil, containers.ErrNotFound
		}
	}

	var results []string

	for _, container := range containersToStop {
		_, err := to.doStopContainer(ctx, container, stoppingOnly)
		if err != nil {
			return results, err
		}

		results = append(results, container.id)
	}

	return results, nil
}

type stopContainerAction string

const stoppingOnly stopContainerAction = "stoppingOnly"
const stopAndRemove stopContainerAction = "stopAndRemove"

func (to *TestContainerOrchestrator) doStopContainer(ctx context.Context, container testContainer, stopAction stopContainerAction) (testContainer, error) {
	if ctx.Err() != nil {
		return container, ctx.Err()
	}

	// Stop is a no-op if the container isn't running
	if container.status != containers.ContainerStatusRunning {
		return container, nil
	}

	container.status = containers.ContainerStatusExited
	container.finishedAt = time.Now().UTC()

	to.containers[container.id] = container

	// Notify listeners that we've stopped the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionStop,
		Actor:  containers.EventActor{ID: container.id},
	})

	if stopAction == stoppingOnly {
		// For testing purposes we want to differentiate between a stop and a remove
		to.containerEventsWatcher.Notify(containers.EventMessage{
			Source: containers.EventSourcePlugin,
			Action: TestEventActionStopWithoutRemove,
			Actor:  containers.EventActor{ID: container.id},
		})
	}

	return container, nil
}

func (to *TestContainerOrchestrator) RemoveContainers(ctx context.Context, names []string, force bool) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var containersToRemove []testContainer
	for _, name := range names {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToRemove = append(containersToRemove, container)
				found = true
			}
		}

		if !found {
			return nil, containers.ErrNotFound
		}
	}

	var results []string

	for i := range containersToRemove {
		container := containersToRemove[i]
		if force {
			stoppedContainer, err := to.doStopContainer(ctx, container, stopAndRemove)
			if err != nil {
				return results, err
			}

			container = stoppedContainer
		}

		id, err := to.doRemoveContainer(ctx, container)
		if err != nil {
			return results, err
		}

		results = append(results, id)
	}

	return results, nil
}

func (to *TestContainerOrchestrator) doRemoveContainer(ctx context.Context, container testContainer) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if container.status != containers.ContainerStatusExited {
		return "", fmt.Errorf("container is not in a state to be removed")
	}

	delete(to.containers, container.id)

	// Notify listeners that we've removed the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionDestroy,
		Actor:  containers.EventActor{ID: container.id},
	})

	return container.id, nil
}

func (to *TestContainerOrchestrator) InspectContainers(ctx context.Context, names []string) ([]containers.InspectedContainer, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var result []containers.InspectedContainer

	for _, name := range names {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				found = true

				inspectedContainer := containers.InspectedContainer{
					Id:         container.id,
					Name:       container.name,
					Image:      container.image,
					CreatedAt:  container.createdAt,
					StartedAt:  container.startedAt,
					FinishedAt: container.finishedAt,
					Status:     container.status,
					ExitCode:   container.exitCode,
					Ports:      container.ports,
					Args:       container.args,
					Env:        container.env,
				}

				for _, networkName := range container.networks {
					network := to.networks[networkName]
					inspectedContainer.Networks = append(inspectedContainer.Networks, containers.InspectedContainerNetwork{
						Id:         network.id,
						Name:       network.name,
						IPAddress:  "127.0.0.1",
						MacAddress: "00:00:00:00:00:00",
						Gateway:    "127.0.0.1",
					})
				}

				result = append(result, inspectedContainer)
			}
		}

		if !found {
			return result, containers.ErrNotFound
		}
	}

	return result, nil
}

func (to *TestContainerOrchestrator) SimulateContainerExit(ctx context.Context, name string) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, container := range to.containers {
		if container.matches(name) {
			container.status = containers.ContainerStatusExited
			container.exitCode = 0
			container.finishedAt = time.Now().UTC()

			to.containers[container.id] = container

			// Notify listeners that we've stopped the container
			to.containerEventsWatcher.Notify(containers.EventMessage{
				Source: containers.EventSourceContainer,
				Action: containers.EventActionStop,
				Actor:  containers.EventActor{ID: container.id},
			})

			return nil
		}
	}

	return containers.ErrNotFound
}

func (to *TestContainerOrchestrator) CaptureContainerLogs(ctx context.Context, name string, stdout io.WriteCloser, stderr io.WriteCloser, options containers.StreamContainerLogsOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	/*
		for _, container := range to.containers {
			if container.matches(name) {
				// TODO: simulate log streaming
			}
		}
	*/

	if stdOutCloseErr := stdout.Close(); stdOutCloseErr != nil {
		to.log.Error(stdOutCloseErr, "closing stdout log destination failed", "Container", name)
	}
	if stdErrCloseErr := stderr.Close(); stdErrCloseErr != nil {
		to.log.Error(stdErrCloseErr, "closing stderr log destination failed", "Container", name)
	}

	return containers.ErrNotFound
}

var _ containers.ContainerOrchestrator = (*TestContainerOrchestrator)(nil)
