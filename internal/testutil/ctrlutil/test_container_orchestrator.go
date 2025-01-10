package ctrlutil

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/davidwartell/go-onecontext/onecontext"
	"github.com/go-logr/logr"
	"github.com/google/uuid"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
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
	networks               map[string]*containerNetwork
	images                 map[string]bool
	imageIds               map[string]string
	imageSecrets           map[string]map[string]string
	containers             map[string]*testContainer
	startupLogs            map[string]containerStartupLogs
	containersToFail       map[string]containerExit
	execs                  map[string]*containerExec
	containerEventsWatcher *pubsub.SubscriptionSet[containers.EventMessage]
	networkEventsWatcher   *pubsub.SubscriptionSet[containers.EventMessage]
	socketServer           *http.Server
	socketFilePath         string
	mutex                  *sync.Mutex
	lifetimeCtx            context.Context
	log                    logr.Logger
}

type containerExit struct {
	exitCode int32
	stdErr   string
}

type containerStartupLogs struct {
	stdout []byte
	stderr []byte
}

type containerExec struct {
	stdout   io.WriteCloser
	stderr   io.WriteCloser
	exited   bool
	exitChan chan int32
}

type TestContainerOrchestratorOption uint32

const (
	TcoOptionNone TestContainerOrchestratorOption = 0

	// Enables communication with the test container orchestrator via a Unix domain socket.
	// Used for tests that involve calls to API server to fetch container logs (the API server is a different process
	// from the the one that runs the container controller and the test container orchestrator).
	TcoOptionEnableSocketListener = 0x01
)

func NewTestContainerOrchestrator(
	lifetimeCtx context.Context,
	log logr.Logger,
	opts TestContainerOrchestratorOption,
) (*TestContainerOrchestrator, error) {
	now := time.Now()

	to := &TestContainerOrchestrator{
		randomNameLength: 20,
		volumes:          map[string]containerVolume{},
		networks: map[string]*containerNetwork{
			"bridge": {
				withId:    newId("bridge"),
				driver:    "bridge",
				isDefault: true,
				created:   now,
			},
			"host": {
				withId:    newId("host"),
				driver:    "host",
				isDefault: true,
				created:   now,
			},
			"none": {
				withId:    newId("none"),
				driver:    "none",
				isDefault: true,
				created:   now,
			},
		},
		images:           map[string]bool{},
		imageIds:         map[string]string{},
		imageSecrets:     map[string]map[string]string{},
		containers:       map[string]*testContainer{},
		execs:            map[string]*containerExec{},
		startupLogs:      map[string]containerStartupLogs{},
		containersToFail: map[string]containerExit{},
		mutex:            &sync.Mutex{},
		lifetimeCtx:      lifetimeCtx,
		log:              log,
	}

	to.containerEventsWatcher = pubsub.NewSubscriptionSet(to.doWatchContainers, lifetimeCtx)
	to.networkEventsWatcher = pubsub.NewSubscriptionSet(to.doWatchNetworks, lifetimeCtx)

	var err error
	if opts&TcoOptionEnableSocketListener != 0 {
		err = setupSocketListener(to)
	}

	return to, err
}

func setupSocketListener(to *TestContainerOrchestrator) error {
	const socketFileSuffixLength = 10
	suffix, suffixErr := randdata.MakeRandomString(socketFileSuffixLength)
	if suffixErr != nil {
		return fmt.Errorf("could not create orchestrator socket file name: %w", suffixErr)
	}
	to.socketFilePath = filepath.Join(usvc_io.DcpTempDir(), fmt.Sprintf("tco_sock_%s", string(suffix)))

	socketListener, listenErr := net.Listen("unix", to.socketFilePath)
	if listenErr != nil {
		_ = os.Remove(to.socketFilePath)
		to.socketFilePath = ""
		return fmt.Errorf("could not create orchestrator socket: %w", listenErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(
		// Use template URL, enabling http.Request.PathValue method
		fmt.Sprintf("GET "+containers.ContainerLogsHttpPath, "{containerId}"),
		func(w http.ResponseWriter, r *http.Request) { to.handleLogRequest(w, r) },
	)
	to.socketServer = &http.Server{Handler: mux}

	go func() {
		serveErr := to.socketServer.Serve(socketListener)
		if serveErr != http.ErrServerClosed {
			to.log.Error(serveErr, "socket server closed unexpectedly")
		}
	}()

	return nil
}

func (to *TestContainerOrchestrator) handleLogRequest(resp http.ResponseWriter, req *http.Request) {
	containerId := req.PathValue("containerId")
	if containerId == "" {
		http.Error(resp, "containerId is required", http.StatusBadRequest)
		return
	}

	to.mutex.Lock()
	matching := slices.Select(maps.Values(to.containers), func(c *testContainer) bool { return c.matches(containerId) })
	to.mutex.Unlock()
	if len(matching) == 0 {
		http.Error(resp, "container not found", http.StatusNotFound)
		return
	}
	if len(matching) > 1 {
		http.Error(resp, "multiple containers found", http.StatusBadRequest)
		return
	}

	logOptions := apiv1.LogOptions{}
	query := req.URL.Query()
	logOptionsErr := apiv1.UrlValuesToLogOptions(&query, &logOptions, nil)
	if logOptionsErr != nil {
		http.Error(resp, logOptionsErr.Error(), http.StatusBadRequest)
		return
	}

	effectiveCtx, cancel := onecontext.Merge(req.Context(), to.lifetimeCtx)
	defer cancel()

	traceId := req.Header.Get("trace-id")
	if traceId == "" {
		http.Error(resp, "TestContainerOrchestrator endpoint requires a trace ID", http.StatusBadRequest)
		return
	}
	requestLog := to.log.WithValues(
		"containerId", containerId,
		"traceId", traceId,
		"source", logOptions.Source,
		"timestamps", logOptions.Timestamps,
		"follow", logOptions.Follow,
	)
	requestLog.V(1).Info("serving container logs")
	innerWriter := testutil.NewLoggingWriteCloser(requestLog, usvc_io.NewFlushWriter(resp))

	var stdoutWriter, stderrWriter usvc_io.NotifyWriteCloser
	switch logOptions.Source {
	case string(apiv1.LogStreamSourceStdout):
		stdoutWriter = usvc_io.NewContextWriteCloser(effectiveCtx, innerWriter)
	case string(apiv1.LogStreamSourceStderr):
		stderrWriter = usvc_io.NewContextWriteCloser(effectiveCtx, innerWriter)
	default:
		// Note that startup logs are handled entirely by the Container log streamer,
		// that is, the are read from files written by the Container controller,
		// and this endpoint/container orchestrator is not involved.
		http.Error(resp, fmt.Sprintf("invalid source '%s'", logOptions.Source), http.StatusBadRequest)
		return
	}

	var containerLogOptions containers.StreamContainerLogsOptions
	containerLogOptions.Timestamps = logOptions.Timestamps
	containerLogOptions.Follow = logOptions.Follow

	resp.Header().Set("Content-Type", "application/octet-stream")
	if flusher, ok := resp.(http.Flusher); ok {
		flusher.Flush()
	}

	capturingErr := to.CaptureContainerLogs(effectiveCtx, containerId, stdoutWriter, stderrWriter, containerLogOptions)
	if capturingErr != nil {
		requestLog.Info("failed to serve logs for container",
			"error", capturingErr.Error(),
		)
		http.Error(resp, capturingErr.Error(), http.StatusInternalServerError)
		return
	}

	if stdoutWriter != nil {
		<-stdoutWriter.Closed()
	} else if stderrWriter != nil {
		<-stderrWriter.Closed()
	} else {
		panic("neither stdoutWriter nor stderrWriter was set")
	}

	requestLog.Info("finished serving container logs")
}

func (to *TestContainerOrchestrator) Close() error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var allCloseErrors error = nil

	if to.socketServer != nil {
		allCloseErrors = errors.Join(allCloseErrors, to.socketServer.Close())
		to.socketServer = nil
	}

	if len(to.socketFilePath) > 0 {
		removeErr := os.Remove(to.socketFilePath)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			allCloseErrors = errors.Join(allCloseErrors, os.Remove(to.socketFilePath))
		}
		to.socketFilePath = ""
	}

	return allCloseErrors
}

func (to *TestContainerOrchestrator) GetSocketFilePath() string {
	return to.socketFilePath
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

func (to *TestContainerOrchestrator) SimulateContainerStartupLogs(name string, stdout, stderr []byte) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	to.startupLogs[name] = containerStartupLogs{stdout: stdout, stderr: stderr}
}

func (to *TestContainerOrchestrator) doWatchContainers(_ context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
}

func (to *TestContainerOrchestrator) doWatchNetworks(_ context.Context, _ *pubsub.SubscriptionSet[containers.EventMessage]) {
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
	created    time.Time
	labels     map[string]string
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
	labels     map[string]string
	stdoutLog  *testutil.BufferWriter
	stderrLog  *testutil.BufferWriter
}

func getID() string {
	return uuid.New().String()
}

func (*TestContainerOrchestrator) IsDefault() bool {
	return true
}

func (*TestContainerOrchestrator) Name() string {
	return "test"
}

func (*TestContainerOrchestrator) ContainerHost() string {
	return "host.test.internal"
}

func (to *TestContainerOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
}

func (to *TestContainerOrchestrator) CheckStatus(_ context.Context, _ containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
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

func (to *TestContainerOrchestrator) WatchNetworks(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	sub := to.networkEventsWatcher.Subscribe(sink)
	return sub, nil
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

	to.networks[options.Name] = &containerNetwork{
		withId:     id,
		driver:     "bridge",
		ipv6:       options.IPv6,
		gateways:   []string{},
		subnets:    []string{},
		containers: []string{},
		created:    time.Now(),
		labels:     options.Labels,
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

				var runningContainers []containers.InspectedNetworkContainer

				for _, id := range network.containers {
					if container, containerFound := to.containers[id]; containerFound {
						if container.status == containers.ContainerStatusRunning {
							runningContainers = append(runningContainers, containers.InspectedNetworkContainer{
								Id:   id,
								Name: to.containers[id].name,
							})
						}
					}
				}

				result = append(result, containers.InspectedNetwork{
					Id:         network.id,
					Name:       network.name,
					Driver:     network.driver,
					IPv6:       network.ipv6,
					Gateways:   network.gateways,
					Subnets:    network.subnets,
					Containers: runningContainers,
					Labels:     map[string]string{},
					CreatedAt:  network.created,
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

func (to *TestContainerOrchestrator) doConnectNetwork(ctx context.Context, network *containerNetwork, container *testContainer) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !slices.Contains(network.containers, container.id) {
		network.containers = append(network.containers, container.id)
		container.networks = append(container.networks, network.name)

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

func (to *TestContainerOrchestrator) ListNetworks(ctx context.Context) ([]containers.ListedNetwork, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	to.mutex.Lock()
	defer to.mutex.Unlock()

	return slices.Map[*containerNetwork, containers.ListedNetwork](
		maps.Values(to.networks),
		func(network *containerNetwork) containers.ListedNetwork {
			return containers.ListedNetwork{
				Created:  network.created,
				Driver:   network.driver,
				ID:       network.id,
				IPv6:     network.ipv6,
				Internal: false,
				Labels:   network.labels,
				Name:     network.name,
			}
		},
	), nil
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
				to.imageSecrets[image][secret.ID] = secret.Value
			}
		}
	}

	return nil
}

func (to *TestContainerOrchestrator) InspectImages(ctx context.Context, images []string) ([]containers.InspectedImage, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	for _, image := range images {
		if _, found := to.images[image]; !found {
			return nil, containers.ErrNotFound
		}
	}

	// TODO: Make image build data inspectable
	return nil, nil
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

func (to *TestContainerOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (*pubsub.Subscription[containers.EventMessage], error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	sub := to.containerEventsWatcher.Subscribe(sink)
	return sub, nil
}

func (to *TestContainerOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	container, err := to.doCreateContainer(ctx, options)
	if err != nil {
		return "", err
	}

	effectiveNetwork := options.Network
	if effectiveNetwork == "" {
		effectiveNetwork = to.DefaultNetworkName()
	}

	if err = to.doConnectNetwork(ctx, to.networks[effectiveNetwork], container); err != nil {
		return container.id, err
	}

	return container.id, nil
}

func (to *TestContainerOrchestrator) doCreateContainer(ctx context.Context, options containers.CreateContainerOptions) (*testContainer, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	name := options.Name
	if name != "" {
		for _, existing := range to.containers {
			if existing.name == name {
				return nil, containers.ErrAlreadyExists
			}
		}
	} else {
		if randomName, err := to.getRandomName(); err != nil {
			return nil, err
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
		networks:  []string{},
		ports:     map[string][]containers.InspectedContainerHostPortConfig{},
		args:      options.Args,
		env:       map[string]string{},
		labels:    map[string]string{},
		stdoutLog: testutil.NewBufferWriter(),
		stderrLog: testutil.NewBufferWriter(),
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
			hostIP = networking.IPv4LocalhostDefaultAddress
		}

		container.ports[fmt.Sprintf("%d/%s", port.ContainerPort, protocol)] = []containers.InspectedContainerHostPortConfig{
			{HostIp: hostIP, HostPort: fmt.Sprintf("%d", hostPort)},
		}
	}

	for _, label := range options.Labels {
		container.labels[label.Key] = label.Value
	}

	to.containers[id.id] = &container

	// Notify listeners that we've created the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionCreate,
		Actor:  containers.EventActor{ID: id.id},
	})

	return &container, nil
}

func (to *TestContainerOrchestrator) StartContainers(ctx context.Context, names []string, streamOptions containers.StreamCommandOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var result []string

	var containersToStart []*testContainer

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

func (to *TestContainerOrchestrator) doStartContainer(ctx context.Context, container *testContainer, streamOptions containers.StreamCommandOptions) (string, error) {
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
			container.stdoutLog.Close()
			container.stderrLog.Close()

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

	for name, containerStartupLogs := range to.startupLogs {
		if container.matches(name) || strings.HasPrefix(container.image, name) {
			var startupLogsWriteErrors error

			if len(containerStartupLogs.stdout) > 0 && streamOptions.StdOutStream != nil {
				_, err := streamOptions.StdOutStream.Write([]byte(containerStartupLogs.stdout))
				startupLogsWriteErrors = errors.Join(startupLogsWriteErrors, err)
			}

			if len(containerStartupLogs.stderr) > 0 && streamOptions.StdErrStream != nil {
				_, err := streamOptions.StdErrStream.Write([]byte(containerStartupLogs.stderr))
				startupLogsWriteErrors = errors.Join(startupLogsWriteErrors, err)
			}

			if startupLogsWriteErrors != nil {
				return "", streamIfPossible(startupLogsWriteErrors)
			} else {
				delete(to.startupLogs, name)
				break
			}
		}
	}

	container.status = containers.ContainerStatusRunning
	container.startedAt = time.Now().UTC()

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

	var cco containers.CreateContainerOptions = containers.CreateContainerOptions(options)
	container, err := to.doCreateContainer(ctx, cco)
	if err != nil {
		return "", err
	}

	effectiveNetwork := options.Network
	if effectiveNetwork == "" {
		effectiveNetwork = to.DefaultNetworkName()
	}

	if err = to.doConnectNetwork(ctx, to.networks[effectiveNetwork], container); err != nil {
		return container.id, err
	}

	if _, err = to.doStartContainer(ctx, container, options.StreamCommandOptions); err != nil {
		return container.id, err
	}

	return container.id, nil
}

func (to *TestContainerOrchestrator) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var foundContainer *testContainer
	for _, container := range to.containers {
		if container.matches(options.Container) {
			foundContainer = container
		}
	}

	if foundContainer == nil {
		return nil, containers.ErrNotFound
	}

	execKey := fmt.Sprintf("%s:%s", foundContainer.id, options.Command)
	if _, found := to.execs[execKey]; found {
		return nil, fmt.Errorf("container '%s' already has command '%s' running", foundContainer.id, options.Command)
	}

	exitCodeChan := make(chan int32, 1)

	to.execs[fmt.Sprintf("%s:%s", foundContainer.id, options.Command)] = &containerExec{
		stdout:   options.StdOutStream,
		stderr:   options.StdErrStream,
		exited:   false,
		exitChan: exitCodeChan,
	}

	return exitCodeChan, nil
}

func (to *TestContainerOrchestrator) StopContainers(ctx context.Context, names []string, secondsToKill uint) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var containersToStop []*testContainer
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
		if err := to.doStopContainer(ctx, container, stoppingOnly); err != nil {
			return results, err
		}

		results = append(results, container.id)
	}

	return results, nil
}

type stopContainerAction string

const stoppingOnly stopContainerAction = "stoppingOnly"
const stopAndRemove stopContainerAction = "stopAndRemove"

func (to *TestContainerOrchestrator) doStopContainer(ctx context.Context, container *testContainer, stopAction stopContainerAction) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stop is a no-op if the container isn't running
	if container.status != containers.ContainerStatusRunning {
		return nil
	}

	for key, exec := range to.execs {
		if !exec.exited && strings.HasPrefix(key, fmt.Sprintf("%s:", container.id)) {
			// If the execution hasn't already been completed, signal that it should be stopped
			exec.exitChan <- -1
			_ = exec.stdout.Close()
			_ = exec.stderr.Close()
			close(exec.exitChan)
		}
	}

	container.status = containers.ContainerStatusExited
	container.finishedAt = time.Now().UTC()
	container.stdoutLog.Close()
	container.stderrLog.Close()

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

	return nil
}

func (to *TestContainerOrchestrator) RemoveContainers(ctx context.Context, names []string, force bool) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(names) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var containersToRemove []*testContainer
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
			if err := to.doStopContainer(ctx, container, stopAndRemove); err != nil {
				return results, err
			}
		}

		id, err := to.doRemoveContainer(ctx, container)
		if err != nil {
			return results, err
		}

		results = append(results, id)
	}

	return results, nil
}

func (to *TestContainerOrchestrator) doRemoveContainer(ctx context.Context, container *testContainer) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if container.status != containers.ContainerStatusExited {
		return "", fmt.Errorf("container is not in a state to be removed")
	}

	delete(to.containers, container.id)

	// Find all executions for the container
	execsToRemove := []string{}
	for key := range to.execs {
		if strings.HasPrefix(key, fmt.Sprintf("%s:", container.id)) {
			execsToRemove = append(execsToRemove, key)
		}
	}

	// Remove all executions for the container
	for _, execToRemove := range execsToRemove {
		delete(to.execs, execToRemove)
	}

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
					Labels:     container.labels,
				}

				for _, networkName := range container.networks {
					network := to.networks[networkName]
					inspectedContainer.Networks = append(inspectedContainer.Networks, containers.InspectedContainerNetwork{
						Id:         network.id,
						Name:       network.name,
						IPAddress:  networking.IPv4LocalhostDefaultAddress,
						MacAddress: "00:00:00:00:00:00",
						Gateway:    networking.IPv4LocalhostDefaultAddress,
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

func (to *TestContainerOrchestrator) SimulateContainerExit(ctx context.Context, name string, exitCode int32) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	for _, container := range to.containers {
		if container.matches(name) {
			if container.status == containers.ContainerStatusExited {
				// Container is already stopped
				return nil
			}

			container.status = containers.ContainerStatusExited
			container.exitCode = exitCode
			container.finishedAt = time.Now().UTC()
			container.stdoutLog.Close()
			container.stderrLog.Close()

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

func (to *TestContainerOrchestrator) SimulateContainerExecExit(ctx context.Context, container string, command string, exitCode int32) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	matching := slices.Select(maps.Values(to.containers), func(tc *testContainer) bool { return tc.matches(container) })
	if len(matching) == 0 {
		return containers.ErrNotFound
	}
	if len(matching) > 1 {
		return fmt.Errorf("multiple containers match the container name")
	}

	var matchingCommand *containerExec
	for key, exec := range to.execs {
		if key == fmt.Sprintf("%s:%s", matching[0].id, command) {
			if matchingCommand != nil {
				return fmt.Errorf("multiple commands match the command name")
			}

			matchingCommand = exec
		}
	}

	if matchingCommand == nil {
		return containers.ErrNotFound
	}

	if matchingCommand.exited {
		return fmt.Errorf("command is not running; only running commands can emit logs")
	}

	matchingCommand.exitChan <- exitCode
	close(matchingCommand.exitChan)
	_ = matchingCommand.stdout.Close()
	_ = matchingCommand.stderr.Close()
	matchingCommand.exited = true

	return nil
}

func (to *TestContainerOrchestrator) CaptureContainerLogs(ctx context.Context, containerNameOrId string, stdout io.WriteCloser, stderr io.WriteCloser, options containers.StreamContainerLogsOptions) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if options.Tail != 0 {
		return fmt.Errorf("tail option is not implemented")
	}
	if !options.Since.IsZero() {
		return fmt.Errorf("since option is not implemented")
	}
	if !options.Until.IsZero() {
		return fmt.Errorf("until option is not implemented")
	}
	if stdout == nil && stderr == nil {
		return fmt.Errorf("at least one of stdout or stderr must be provided")
	}

	to.mutex.Lock()
	matching := slices.Select(maps.Values(to.containers), func(tc *testContainer) bool { return tc.matches(containerNameOrId) })
	to.mutex.Unlock()
	if len(matching) == 0 {
		return containers.ErrNotFound
	}
	if len(matching) > 1 {
		return fmt.Errorf("multiple containers match the name")
	}
	tc := matching[0]

	var effectiveStdout, effecitveStderr io.WriteCloser
	if options.Timestamps {
		if stdout != nil {
			effectiveStdout = usvc_io.NewTimestampWriter(stdout)
		}
		if stderr != nil {
			effecitveStderr = usvc_io.NewTimestampWriter(stderr)
		}
	} else {
		effectiveStdout = stdout
		effecitveStderr = stderr
	}

	// Account for the possibility that the capturing context, or the orchestrator lifetime context, may be cancelled.
	if effectiveStdout != nil {
		effectiveStdout = usvc_io.NewContextWriteCloser(ctx, effectiveStdout)
	}
	if effecitveStderr != nil {
		effecitveStderr = usvc_io.NewContextWriteCloser(ctx, effecitveStderr)
	}

	var logErrors error
	if !options.Follow {
		if effectiveStdout != nil {
			if _, stdOutWriteErr := effectiveStdout.Write(tc.stdoutLog.Bytes()); stdOutWriteErr != nil {
				logErrors = errors.Join(logErrors, stdOutWriteErr)
			} else {
				logErrors = errors.Join(logErrors, effectiveStdout.Close())
			}
		}

		if effecitveStderr != nil {
			if _, stdErrWriteErr := effecitveStderr.Write(tc.stderrLog.Bytes()); stdErrWriteErr != nil {
				logErrors = errors.Join(logErrors, stdErrWriteErr)
			} else {
				logErrors = errors.Join(logErrors, effecitveStderr.Close())
			}
		}
	} else {
		if effectiveStdout != nil {
			logErrors = errors.Join(logErrors, tc.stdoutLog.AddTarget(effectiveStdout))
		}
		if effecitveStderr != nil {
			logErrors = errors.Join(logErrors, tc.stderrLog.AddTarget(effecitveStderr))
		}
	}

	return logErrors
}

func (to *TestContainerOrchestrator) SimulateContainerLogging(containerName string, target apiv1.LogStreamSource, content []byte) error {
	to.mutex.Lock()
	matching := slices.Select(maps.Values(to.containers), func(tc *testContainer) bool { return tc.matches(containerName) })
	to.mutex.Unlock()
	if len(matching) == 0 {
		return containers.ErrNotFound
	}
	if len(matching) > 1 {
		return fmt.Errorf("multiple containers match the name")
	}
	tc := matching[0]

	if tc.status != containers.ContainerStatusRunning {
		return fmt.Errorf("container is not running; only running containers can emit logs")
	}

	var writer io.Writer
	switch target {
	case apiv1.LogStreamSourceStdout:
		writer = tc.stdoutLog
	case apiv1.LogStreamSourceStderr:
		writer = tc.stderrLog
	default:
		return fmt.Errorf("only stdout and stderr log targets are supported")
	}

	_, writeErr := writer.Write(content)
	return writeErr
}

func (to *TestContainerOrchestrator) SimulateContainerExecCommandLogging(container string, command string, target apiv1.LogStreamSource, content []byte) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()
	matching := slices.Select(maps.Values(to.containers), func(tc *testContainer) bool { return tc.matches(container) })
	if len(matching) == 0 {
		return containers.ErrNotFound
	}
	if len(matching) > 1 {
		return fmt.Errorf("multiple containers match the container name")
	}

	var matchingCommand *containerExec
	for key, exec := range to.execs {
		if key == fmt.Sprintf("%s:%s", matching[0].id, command) {
			if matchingCommand != nil {
				return fmt.Errorf("multiple commands match the command name")
			}

			matchingCommand = exec
		}
	}

	if matchingCommand == nil {
		return containers.ErrNotFound
	}

	if matchingCommand.exited {
		return fmt.Errorf("command is not running; only running commands can emit logs")
	}

	var writer io.Writer
	switch target {
	case apiv1.LogStreamSourceStdout:
		writer = matchingCommand.stdout
	case apiv1.LogStreamSourceStderr:
		writer = matchingCommand.stderr
	default:
		return fmt.Errorf("only stdout and stderr log targets are supported")
	}

	_, writeErr := writer.Write(content)
	return writeErr
}

var _ containers.ContainerOrchestrator = (*TestContainerOrchestrator)(nil)
var _ io.Closer = (*TestContainerOrchestrator)(nil)
