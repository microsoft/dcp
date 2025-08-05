package ctrlutil

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	randomNameEncoder   = base32.HexEncoding.WithPadding(base32.NoPadding)
	errRuntimeUnhealthy = errors.New("(test) container runtime is unhealthy")

	ContainerNameAttribute string = apiv1.GroupVersion.Group + ".container_name"
)

const (
	TestEventActionStopWithoutRemove containers.EventAction = "stop_without_remove"
	randomNameLength                 int                    = 20

	// Range of ports used for "host port" if container port configuration does not specify a host port.
	// (Docker/Podman assigns a random port in this case)
	// Note that these ports are not actually used during the test (no sockets are bound to them),
	// they are just for verification purposes.
	MinRandomHostPort int = 40001
	MaxRandomHostPort int = 50000
)

type TestContainerOrchestrator struct {
	runtimeHealthy          bool
	volumes                 map[string]containerVolume
	networks                map[string]*containerNetwork
	images                  []*testImage
	containers              map[string]*testContainer
	startupLogs             map[string]containerStartupLogs
	containersToFail        map[string]containerExit
	containersToHealthcheck map[string]containers.ContainerHealthcheck
	execs                   map[string]*containerExec
	createFiles             map[string][]*containerCreateFile
	containerEventsWatcher  *pubsub.SubscriptionSet[containers.EventMessage]
	networkEventsWatcher    *pubsub.SubscriptionSet[containers.EventMessage]
	socketServer            *http.Server
	socketFilePath          string
	mutex                   *sync.Mutex
	lifetimeCtx             context.Context
	log                     logr.Logger
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

type containerCreateFile struct {
	Destination  string
	Umask        fs.FileMode
	DefaultOwner int32
	DefaultGroup int32
	ModTime      time.Time
	Tar          *bytes.Buffer
}

func (ccf *containerCreateFile) GetTarItems() ([]*tar.Header, error) {
	headers := []*tar.Header{}
	reader := tar.NewReader(ccf.Tar)
	for {
		header, err := reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, err
			}

			break
		}

		headers = append(headers, header)
	}

	return headers, nil
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
		runtimeHealthy: true,
		volumes:        map[string]containerVolume{},
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
		containers:              map[string]*testContainer{},
		execs:                   map[string]*containerExec{},
		startupLogs:             map[string]containerStartupLogs{},
		containersToFail:        map[string]containerExit{},
		containersToHealthcheck: map[string]containers.ContainerHealthcheck{},
		createFiles:             map[string][]*containerCreateFile{},
		mutex:                   &sync.Mutex{},
		lifetimeCtx:             lifetimeCtx,
		log:                     log,
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
	if !to.runtimeHealthy {
		http.Error(resp, errRuntimeUnhealthy.Error(), http.StatusInternalServerError)
		return
	}

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
		"options", logOptions.String(),
	)
	requestLog.V(1).Info("serving container logs")
	innerWriter := NewLoggingWriteCloser(requestLog, resp)

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

func (to *TestContainerOrchestrator) SetHealthcheckMatchingContainers(ctx context.Context, name string, healthcheck containers.ContainerHealthcheck) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	to.containersToHealthcheck[name] = healthcheck

	go func() {
		<-ctx.Done()
		to.mutex.Lock()
		defer to.mutex.Unlock()

		delete(to.containersToHealthcheck, name)
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
	postfixBytes := make([]byte, randomNameLength)
	if read, err := rand.Read(postfixBytes); err != nil {
		return "", err
	} else if read != randomNameLength {
		return "", fmt.Errorf("could not generate %d bytes of randomness", randomNameLength)
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

type TestContainerPortConfig struct {
	containers.InspectedContainerHostPortConfig
	UseRandomHostPort bool
}

type testContainer struct {
	withId
	image       string
	createdAt   time.Time
	startedAt   time.Time
	finishedAt  time.Time
	status      containers.ContainerStatus
	exitCode    int32
	ports       map[string][]TestContainerPortConfig
	networks    []string
	args        []string
	env         map[string]string
	labels      map[string]string
	health      *containers.InspectedContainerHealth
	healthcheck containers.ContainerHealthcheck
	stdoutLog   *testutil.BufferWriter
	stderrLog   *testutil.BufferWriter
}

type testImage struct {
	id      string
	digest  string
	tags    []string
	secrets map[string]string
	labels  map[string]string
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
		Running:   to.runtimeHealthy,
	}
}

func (to *TestContainerOrchestrator) SetRuntimeHealth(healthy bool) {
	to.mutex.Lock()
	defer to.mutex.Unlock()
	to.runtimeHealthy = healthy
}

// GetDiagnostics returns an empty diagnostics object, as the test container orchestrator does not support diagnostics.
func (to *TestContainerOrchestrator) GetDiagnostics(ctx context.Context) (containers.ContainerDiagnostics, error) {
	return containers.ContainerDiagnostics{}, nil
}

func (to *TestContainerOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

	if _, found := to.volumes[options.Name]; found {
		return containers.ErrAlreadyExists
	}

	to.volumes[options.Name] = containerVolume{name: options.Name, created: time.Now().UTC()}

	return nil
}

func (to *TestContainerOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var removed []string

	var err error
	for _, name := range options.Volumes {
		if _, found := to.volumes[name]; !found {
			err = errors.Join(err, containers.ErrNotFound)
			continue
		}

		removed = append(removed, name)
	}

	for _, name := range removed {
		delete(to.volumes, name)
	}

	if len(removed) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all volumes were removed, expected %d but got %d", len(options.Volumes), len(removed))))
	}

	return removed, err
}

func (to *TestContainerOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var result []containers.InspectedVolume

	var err error
	for _, name := range options.Volumes {
		volume, found := to.volumes[name]
		if !found {
			err = errors.Join(err, containers.ErrNotFound)
			continue
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

	if len(result) < len(options.Volumes) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all volumes were inspected, expected %d but got %d", len(options.Volumes), len(result))))
	}

	return result, err
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

	if !to.runtimeHealthy {
		return "", errRuntimeUnhealthy
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

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var err error
	names := []string{}
	ids := []string{}
	for _, name := range options.Networks {
		for _, network := range to.networks {
			if network.matches(name) {
				if network.isDefault {
					err = errors.Join(err, fmt.Errorf("cannot remove default network: %s", name))
					continue
				}

				if len(network.containers) > 0 {
					err = errors.Join(err, fmt.Errorf("cannot remove network with containers"))
					continue
				}

				names = append(names, network.name)
				ids = append(ids, network.id)
			}

			if !options.Force {
				err = errors.Join(err, fmt.Errorf("network %s not found", name))
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

	if len(ids) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all networks were removed, expected %d but got %d", len(options.Networks), len(ids))))
	}

	return ids, err
}

func (to *TestContainerOrchestrator) InspectNetworks(ctx context.Context, options containers.InspectNetworksOptions) ([]containers.InspectedNetwork, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var result []containers.InspectedNetwork

	var err error
	for _, name := range options.Networks {
		var found bool
		for _, network := range to.networks {
			if network.matches(name) {
				found = true

				var connectedContainers []containers.InspectedNetworkContainer

				for _, id := range network.containers {
					if _, containerFound := to.containers[id]; containerFound {
						connectedContainers = append(connectedContainers, containers.InspectedNetworkContainer{
							Id:   id,
							Name: to.containers[id].name,
						})
					}
				}

				result = append(result, containers.InspectedNetwork{
					Id:         network.id,
					Name:       network.name,
					Driver:     network.driver,
					IPv6:       network.ipv6,
					Gateways:   network.gateways,
					Subnets:    network.subnets,
					Containers: connectedContainers,
					Labels:     map[string]string{},
					CreatedAt:  network.created,
				})
			}
		}

		if !found {
			err = errors.Join(err, containers.ErrNotFound)
		}
	}

	if len(result) < len(options.Networks) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all networks were inspected, expected %d but got %d", len(options.Networks), len(result))))
	}

	return result, err
}

func (to *TestContainerOrchestrator) ConnectNetwork(ctx context.Context, options containers.ConnectNetworkOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
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

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
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

func (to *TestContainerOrchestrator) ListNetworks(ctx context.Context, options containers.ListNetworksOptions) ([]containers.ListedNetwork, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	to.mutex.Lock()
	defer to.mutex.Unlock()

	filteredNetworks := slices.Select(maps.Values(to.networks), func(network *containerNetwork) bool {
		// If there are no label filters, we can include the network as it satisfied all other filters.
		if len(options.Filters.LabelFilters) == 0 {
			return true
		}

		if slices.Any(options.Filters.LabelFilters, func(label containers.LabelFilter) bool {
			if value, found := network.labels[label.Key]; found && label.Value == "" || value == label.Value {
				// If the label is present and matches the value, we want to include the network.
				return true
			}

			return false
		}) {
			// One of the label filters matched, so we include this network.
			return true
		}

		return false
	})

	return slices.Map[*containerNetwork, containers.ListedNetwork](
		filteredNetworks,
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

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

	guid := uuid.New().String()
	image := &testImage{
		id:      guid,
		digest:  toDigest(sha256.Sum256([]byte(guid))),
		tags:    options.Tags,
		secrets: map[string]string{},
		labels: maps.SliceToMap(options.Labels, func(label apiv1.ContainerLabel) (string, string) {
			return label.Key, label.Value
		}),
	}

	for _, secret := range options.Secrets {
		if secret.Type == apiv1.EnvSecret && secret.Value != "" {
			image.secrets[secret.ID] = secret.Value
		}
	}

	to.images = append(to.images, image)

	if options.IidFile != "" {
		err := usvc_io.WriteFile(options.IidFile, []byte(guid), osutil.PermissionOwnerReadWriteOthersRead)
		if err != nil {
			return err
		}
	}

	return nil
}

func (to *TestContainerOrchestrator) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var err error
	var result []containers.InspectedImage

	for _, imageId := range options.Images {
		image, found := to.findImage(imageId)
		if !found {
			err = errors.Join(err, containers.ErrNotFound)
			continue
		}

		// CONSIDER: surface mock image build data via inspection

		result = append(result, containers.InspectedImage{
			Id:     image.id,
			Labels: image.labels,
			Tags:   image.tags,
			Digest: image.digest,
		})
	}

	if len(result) < len(options.Images) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all images were inspected, expected %d but got %d", len(options.Images), len(result))))
	}

	return result, err
}

func (to *TestContainerOrchestrator) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if !to.runtimeHealthy {
		return "", errRuntimeUnhealthy
	}

	image, found := to.findImage(options.Image)
	if found {
		return image.id, nil
	}

	// For now we pretend that all pulls are successful.

	guid := uuid.New().String()
	image = &testImage{
		id:      guid,
		tags:    []string{options.Image},
		secrets: map[string]string{},
	}
	if options.Digest != "" {
		image.digest = options.Digest
	} else {
		image.digest = toDigest(sha256.Sum256([]byte(guid)))
	}

	to.images = append(to.images, image)

	return image.id, nil
}

func (to *TestContainerOrchestrator) HasImage(id string) bool {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	_, found := to.findImage(id)
	return found
}

func (to *TestContainerOrchestrator) findImage(id string) (*testImage, bool) {
	i := slices.IndexFunc(to.images, func(img *testImage) bool {
		return img.id == id || slices.Contains(img.tags, id)
	})
	if i >= 0 {
		return to.images[i], true
	} else {
		return nil, false
	}
}

func toDigest(sha [32]byte) string {
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sha[:]))
}

func (to *TestContainerOrchestrator) ImageCount() int {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	return len(to.images)
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

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if !to.runtimeHealthy {
		return "", errRuntimeUnhealthy
	}

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
		withId:      id,
		image:       options.Image,
		createdAt:   time.Now().UTC(),
		status:      containers.ContainerStatusCreated,
		networks:    []string{},
		ports:       map[string][]TestContainerPortConfig{},
		args:        options.Args,
		env:         map[string]string{},
		labels:      map[string]string{},
		healthcheck: options.Healthcheck,
		stdoutLog:   testutil.NewBufferWriter(),
		stderrLog:   testutil.NewBufferWriter(),
	}

	for ctr, healthcheck := range to.containersToHealthcheck {
		if container.matches(ctr) {
			container.healthcheck = healthcheck
			break
		}
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
			i, randErr := randdata.MakeRandomInt64(int64(MaxRandomHostPort) - int64(MinRandomHostPort))
			if randErr != nil {
				return nil, randErr
			}
			hostPort = int32(MinRandomHostPort) + int32(i)
		}

		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = networking.IPv4LocalhostDefaultAddress
		}

		container.ports[fmt.Sprintf("%d/%s", port.ContainerPort, protocol)] = []TestContainerPortConfig{
			{
				InspectedContainerHostPortConfig: containers.InspectedContainerHostPortConfig{
					HostIp:   hostIP,
					HostPort: fmt.Sprintf("%d", hostPort),
				},
				UseRandomHostPort: port.HostPort == 0,
			},
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
		Attributes: map[string]string{
			ContainerNameAttribute: name,
		},
	})

	return &container, nil
}

func (to *TestContainerOrchestrator) StartContainers(ctx context.Context, options containers.StartContainersOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var result []string

	var containersToStart []*testContainer

	var err error
	for _, name := range options.Containers {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToStart = append(containersToStart, container)
				found = true
			}
		}

		if !found {
			err = errors.Join(err, containers.ErrNotFound)
		}
	}

	for _, container := range containersToStart {
		id, startErr := to.doStartContainer(ctx, container, options.StreamCommandOptions)
		if startErr != nil {
			err = errors.Join(err, startErr)
			continue
		}

		result = append(result, id)
	}

	if len(result) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were started, expected %d but got %d", len(options.Containers), len(result))))
	}

	return result, err
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

	if container.status != containers.ContainerStatusCreated && container.status != containers.ContainerStatusExited && container.status != containers.ContainerStatusPaused {
		return "", streamIfPossible(fmt.Errorf("container is not in a state to be started"))
	}

	for name, exit := range to.containersToFail {
		if container.matches(name) || strings.HasPrefix(container.image, name) {
			return container.id, streamIfPossible(fmt.Errorf("container failed to start: %s", exit.stdErr))
		}
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
		Attributes: map[string]string{
			ContainerNameAttribute: container.name,
		},
	})

	return container.id, nil
}

func (to *TestContainerOrchestrator) RunContainer(ctx context.Context, options containers.RunContainerOptions) (string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if !to.runtimeHealthy {
		return "", errRuntimeUnhealthy
	}

	container, err := to.doCreateContainer(ctx, options.CreateContainerOptions)
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

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

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

func (to *TestContainerOrchestrator) StopContainers(ctx context.Context, options containers.StopContainersOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var err error
	var containersToStop []*testContainer
	for _, name := range options.Containers {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToStop = append(containersToStop, container)
				found = true
			}
		}

		if !found {
			err = errors.Join(err, containers.ErrNotFound)
		}
	}

	var results []string

	for _, container := range containersToStop {
		if stopErr := to.doStopContainer(ctx, container, stoppingOnly); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to stop container '%s': %w", container.id, stopErr))
			continue
		}

		results = append(results, container.id)
	}

	if len(results) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were stopped, expected %d but got %d", len(options.Containers), len(results))))
	}

	return results, err
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
		Attributes: map[string]string{
			ContainerNameAttribute: container.name,
		},
	})

	if stopAction == stoppingOnly {
		// For testing purposes we want to differentiate between a stop and a remove
		to.containerEventsWatcher.Notify(containers.EventMessage{
			Source: containers.EventSourcePlugin,
			Action: TestEventActionStopWithoutRemove,
			Actor:  containers.EventActor{ID: container.id},
			Attributes: map[string]string{
				ContainerNameAttribute: container.name,
			},
		})
	}

	return nil
}

func (to *TestContainerOrchestrator) RemoveContainers(ctx context.Context, options containers.RemoveContainersOptions) ([]string, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var err error
	var containersToRemove []*testContainer
	for _, name := range options.Containers {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				containersToRemove = append(containersToRemove, container)
				found = true
			}
		}

		if !found {
			err = errors.Join(err, containers.ErrNotFound)
		}
	}

	var results []string

	for i := range containersToRemove {
		container := containersToRemove[i]
		if options.Force {
			if stopErr := to.doStopContainer(ctx, container, stopAndRemove); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to stop container '%s': %w", container.id, stopErr))
				continue
			}
		}

		id, removeErr := to.doRemoveContainer(ctx, container)
		if removeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to remove container '%s': %w", container.id, removeErr))
			continue
		}

		results = append(results, id)
	}

	if len(results) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were removed, expected %d but got %d", len(options.Containers), len(results))))
	}

	return results, err
}

func (to *TestContainerOrchestrator) doRemoveContainer(ctx context.Context, container *testContainer) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if container.status != containers.ContainerStatusExited && container.status != containers.ContainerStatusCreated && container.status != containers.ContainerStatusDead {
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

	// Detach the container being deleted from all networks
	for _, network := range to.networks {
		remaining, _ := slices.Diff(network.containers, []string{container.id})
		network.containers = remaining
	}

	// Notify listeners that we've removed the container
	to.containerEventsWatcher.Notify(containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionDestroy,
		Actor:  containers.EventActor{ID: container.id},
		Attributes: map[string]string{
			ContainerNameAttribute: container.name,
		},
	})

	return container.id, nil
}

func (to *TestContainerOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	filteredContainers := slices.Select(maps.Values(to.containers), func(container *testContainer) bool {
		// If there are no label filters, we should include all containers.
		if len(options.Filters.LabelFilters) == 0 {
			return true
		}

		if slices.Any(options.Filters.LabelFilters, func(label containers.LabelFilter) bool {
			if value, found := container.labels[label.Key]; found && label.Value == "" || value == label.Value {
				// If the label is present and matches the value, we want to include the network.
				return true
			}

			return false
		}) {
			// One of the label filters matched, so we include this network.
			return true
		}

		// We didn't match any of the label filters, so we don't include this container.
		return false
	})

	return slices.Map[*testContainer, containers.ListedContainer](
		filteredContainers,
		func(container *testContainer) containers.ListedContainer {
			return containers.ListedContainer{
				Id:       container.id,
				Name:     container.name,
				Image:    container.image,
				Status:   container.status,
				Labels:   container.labels,
				Networks: container.networks,
			}
		},
	), nil
}

func (to *TestContainerOrchestrator) InspectContainers(ctx context.Context, options containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !to.runtimeHealthy {
		return nil, errRuntimeUnhealthy
	}

	var result []containers.InspectedContainer

	var err error
	for _, name := range options.Containers {
		var found bool
		for _, container := range to.containers {
			if container.matches(name) {
				found = true

				inspectedContainerPorts := maps.Map[string, []TestContainerPortConfig, []containers.InspectedContainerHostPortConfig](container.ports, func(key string, tcp []TestContainerPortConfig) []containers.InspectedContainerHostPortConfig {
					retval := make([]containers.InspectedContainerHostPortConfig, len(tcp))
					for i, value := range tcp {
						retval[i] = containers.InspectedContainerHostPortConfig{
							HostIp:   value.HostIp,
							HostPort: value.HostPort,
						}
					}
					return retval
				})

				inspectedContainer := containers.InspectedContainer{
					Id:          container.id,
					Name:        container.name,
					Image:       container.image,
					CreatedAt:   container.createdAt,
					StartedAt:   container.startedAt,
					FinishedAt:  container.finishedAt,
					Status:      container.status,
					ExitCode:    container.exitCode,
					Ports:       inspectedContainerPorts,
					Args:        container.args,
					Env:         container.env,
					Labels:      container.labels,
					Healthcheck: container.healthcheck.Command,
					Health:      container.health,
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
			err = errors.Join(err, containers.ErrNotFound)
		}
	}

	if len(result) < len(options.Containers) {
		err = errors.Join(err, errors.Join(containers.ErrIncomplete, fmt.Errorf("not all containers were inspected, expected %d but got %d", len(options.Containers), len(result))))
	}

	return result, err
}

func (to *TestContainerOrchestrator) CreateFiles(ctx context.Context, options containers.CreateFilesOptions) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

	var foundContainer *testContainer
	for _, container := range to.containers {
		if container.matches(options.Container) {
			foundContainer = container
		}
	}

	if foundContainer == nil {
		return containers.ErrNotFound
	}

	tarWriter := usvc_io.NewTarWriter()

	for _, item := range options.Entries {
		if item.Type == apiv1.FileSystemEntryTypeDir {
			if addDirectoryErr := containers.AddDirectoryToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, to.log); addDirectoryErr != nil {
				return addDirectoryErr
			}
		} else {
			if addFileErr := containers.AddFileToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, to.log); addFileErr != nil {
				return addFileErr
			}
		}
	}

	buffer, bufferErr := tarWriter.Buffer()
	if bufferErr != nil {
		return bufferErr
	}

	to.createFiles[foundContainer.id] = append(to.createFiles[foundContainer.id], &containerCreateFile{
		Destination:  options.Destination,
		Umask:        options.Umask,
		DefaultOwner: options.DefaultOwner,
		DefaultGroup: options.DefaultGroup,
		ModTime:      options.ModTime,
		Tar:          buffer,
	})

	return nil
}

func (to *TestContainerOrchestrator) GetCreatedFiles(name string) ([]*containerCreateFile, error) {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	var foundContainer *testContainer
	for _, container := range to.containers {
		if container.matches(name) {
			foundContainer = container
		}
	}

	if foundContainer == nil {
		return nil, containers.ErrNotFound
	}

	return to.createFiles[foundContainer.id], nil
}

func (to *TestContainerOrchestrator) SimulateHealthcheck(ctx context.Context, name string, health *containers.InspectedContainerHealth) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

	for _, container := range to.containers {
		if container.matches(name) {
			container.health = health
			to.containers[container.id] = container

			// Notify listeners that we've updated the health of the container
			to.containerEventsWatcher.Notify(containers.EventMessage{
				Source: containers.EventSourceContainer,
				Action: containers.EventActionHealthStatus,
				Actor:  containers.EventActor{ID: container.id},
				Attributes: map[string]string{
					ContainerNameAttribute: name,
				},
			})

			return nil
		}
	}

	return containers.ErrNotFound
}

func (to *TestContainerOrchestrator) SimulateContainerExit(ctx context.Context, name string, exitCode int32) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
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
				Attributes: map[string]string{
					ContainerNameAttribute: name,
				},
			})

			return nil
		}
	}

	return containers.ErrNotFound
}

func (to *TestContainerOrchestrator) SimulateContainerRestart(ctx context.Context, name string) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

	for _, container := range to.containers {
		if container.matches(name) {
			if container.status != containers.ContainerStatusRunning {
				return fmt.Errorf("container is not running and cannot be restarted")
			}

			container.status = containers.ContainerStatusRunning

			// Simulate remapping of randomly assigned ports
			newPorts := map[string][]TestContainerPortConfig{}

			for key, portConfigs := range container.ports {
				newPortConfigs := []TestContainerPortConfig{}

				for _, portConfig := range portConfigs {
					pc := portConfig
					if portConfig.UseRandomHostPort {
						i, randErr := randdata.MakeRandomInt64(int64(MaxRandomHostPort) - int64(MinRandomHostPort))
						if randErr != nil {
							return randErr
						}
						pc.HostPort = fmt.Sprintf("%d", int32(MinRandomHostPort)+int32(i))
					}
					newPortConfigs = append(newPortConfigs, pc)
				}

				newPorts[key] = newPortConfigs
			}

			container.ports = newPorts

			// Notify listeners that we've restarted the container
			to.containerEventsWatcher.Notify(containers.EventMessage{
				Source: containers.EventSourceContainer,
				Action: containers.EventActionRestart,
				Actor:  containers.EventActor{ID: container.id},
				Attributes: map[string]string{
					ContainerNameAttribute: name,
				},
			})

			return nil
		}
	}

	return containers.ErrNotFound
}

func (to *TestContainerOrchestrator) SimulateContainerExecExit(ctx context.Context, container string, command string, exitCode int32) error {
	to.mutex.Lock()
	defer to.mutex.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
	}

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

func (to *TestContainerOrchestrator) CaptureContainerLogs(ctx context.Context, containerNameOrId string, stdout usvc_io.WriteSyncerCloser, stderr usvc_io.WriteSyncerCloser, options containers.StreamContainerLogsOptions) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if !to.runtimeHealthy {
		return errRuntimeUnhealthy
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

	var effectiveStdout, effectiveStderr usvc_io.WriteSyncerCloser
	if options.Timestamps {
		if stdout != nil {
			effectiveStdout = usvc_io.NewTimestampWriter(stdout)
		}
		if stderr != nil {
			effectiveStderr = usvc_io.NewTimestampWriter(stderr)
		}
	} else {
		effectiveStdout = stdout
		effectiveStderr = stderr
	}

	// Account for the possibility that the capturing context, or the orchestrator lifetime context, may be cancelled.
	if effectiveStdout != nil {
		effectiveStdout = usvc_io.NewContextWriteCloser(ctx, effectiveStdout)
	}
	if effectiveStderr != nil {
		effectiveStderr = usvc_io.NewContextWriteCloser(ctx, effectiveStderr)
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

		if effectiveStderr != nil {
			if _, stdErrWriteErr := effectiveStderr.Write(tc.stderrLog.Bytes()); stdErrWriteErr != nil {
				logErrors = errors.Join(logErrors, stdErrWriteErr)
			} else {
				logErrors = errors.Join(logErrors, effectiveStderr.Close())
			}
		}
	} else {
		if effectiveStdout != nil {
			logErrors = errors.Join(logErrors, tc.stdoutLog.AddTarget(effectiveStdout))
		}
		if effectiveStderr != nil {
			logErrors = errors.Join(logErrors, tc.stderrLog.AddTarget(effectiveStderr))
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
