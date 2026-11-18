/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	container_runtimes "github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
)

// projectDebugRuntime owns adapter processes across requests; TCP-connect providers are shared by Project UID.
type projectDebugRuntime struct {
	ctx                  context.Context
	executor             process.Executor
	writer               *projectDebugStatusWriter
	log                  logr.Logger
	launches             chan struct{}
	cleanups             chan struct{}
	mu                   sync.Mutex
	providers            map[types.UID]*projectDebugProvider
	findContainerRuntime func(context.Context, string) (containers.ExecContainers, error)
}

type projectDebugProvider struct {
	ready     chan struct{}
	stopped   chan struct{}
	refs      int
	stopping  bool
	fenced    bool
	execution *projectDebugExecution
	initial   *projectDebugTransport
	err       error
	cancel    context.CancelFunc
}

type projectDebugExecution struct {
	runtime     *projectDebugRuntime
	projectName types.NamespacedName
	projectUID  types.UID
	generation  int64
	cancel      context.CancelFunc
	handle      process.ProcessHandle
	address     string
	done        chan struct{}
	statusMu    sync.Mutex
	status      apiv2.ProjectDebugExecutionStatus
	containerID string
	stopOnce    sync.Once
	stopErr     error
}

type projectDebugTransport struct {
	io.Reader
	io.Writer
	close    func() error
	failure  context.Context
	readDone <-chan struct{}
	once     sync.Once
	err      error
}

func (transport *projectDebugTransport) Close() error {
	transport.once.Do(func() {
		transport.err = transport.close()
		if errors.Is(transport.err, os.ErrClosed) || errors.Is(transport.err, net.ErrClosed) || errors.Is(transport.err, io.ErrClosedPipe) {
			transport.err = nil
		}
	})
	return transport.err
}

type adapterDiagnosticWriter struct{ log logr.Logger }

func (writer adapterDiagnosticWriter) Write(payload []byte) (int, error) {
	writer.log.Info("Debug adapter output", "Output", string(payload))
	return len(payload), nil
}

func newProjectDebugRuntime(ctx context.Context, executor process.Executor, writer *projectDebugStatusWriter, log logr.Logger) *projectDebugRuntime {
	return &projectDebugRuntime{
		ctx: ctx, executor: executor, writer: writer, log: log,
		launches: make(chan struct{}, 8), cleanups: make(chan struct{}, 8), providers: make(map[types.UID]*projectDebugProvider),
		findContainerRuntime: func(lookupCtx context.Context, name string) (containers.ExecContainers, error) {
			return container_runtimes.FindContainerRuntime(lookupCtx, name, log, executor)
		},
	}
}

func (runtime *projectDebugRuntime) publish(
	project *apiv2.Project, execution *apiv2.ProjectDebugExecutionStatus, session *apiv2.ProjectDebugSessionStatus,
) error {
	publishCtx, cancel := process.WithDetachedStopTimeout(runtime.ctx)
	defer cancel()
	return resiliency.RetryExponential(publishCtx, func() error {
		_, updateErr := runtime.writer.update(publishCtx, project.NamespacedName(), project.UID, execution, session)
		if apierrors.IsNotFound(updateErr) || apierrors.IsConflict(updateErr) || apierrors.IsForbidden(updateErr) {
			return resiliency.Permanent(updateErr)
		}
		return updateErr
	})
}

func (execution *projectDebugExecution) stop() error {
	execution.stopOnce.Do(func() {
		execution.runtime.cleanups <- struct{}{}
		defer func() { <-execution.runtime.cleanups }()
		execution.stopErr = execution.stopExecution()
	})
	return execution.stopErr
}

func (execution *projectDebugExecution) stopExecution() error {
	if execution.containerID != "" {
		return execution.stopContainer()
	}
	cleanupCtx, cancel := process.WithDetachedStopTimeout(execution.runtime.ctx)
	defer cancel()
	stopErr := execution.runtime.executor.StopProcess(cleanupCtx, execution.handle)
	if process.IsProcessGoneErr(stopErr) && !errors.Is(stopErr, process.ErrIncompleteProcessTree) {
		stopErr = nil
	}
	execution.cancel()
	select {
	case <-execution.done:
	case <-cleanupCtx.Done():
		stopErr = errors.Join(stopErr, cleanupCtx.Err())
	}
	runningErr := execution.runtime.executor.CheckProcessRunning(execution.handle)
	if runningErr == nil || !process.IsProcessGoneErr(runningErr) {
		stopErr = errors.Join(stopErr, runningErr, fmt.Errorf("adapter termination could not be confirmed"))
		execution.statusMu.Lock()
		execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.MicroTime{}
		execution.status.Conditions = debugConditions("StopFailed", stopErr.Error(), false, execution.generation)
		project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{
			Name: execution.projectName.Name, Namespace: execution.projectName.Namespace, UID: execution.projectUID,
		}}
		publishErr := execution.runtime.publish(project, execution.status.DeepCopy(), nil)
		execution.statusMu.Unlock()
		stopErr = errors.Join(stopErr, publishErr)
	} else {
		execution.statusMu.Lock()
		publishCompletion := execution.status.FinishedAt.IsZero()
		if publishCompletion {
			execution.status.FinishedAt = metav1.NowMicro()
		}
		if errors.Is(stopErr, process.ErrIncompleteProcessTree) {
			execution.status.Phase = apiv2.ProjectDebugPhaseExited
			execution.status.Conditions = debugConditions("DescendantCleanupUnconfirmed",
				fmt.Sprintf("The adapter root exited, but descendant cleanup could not be confirmed: %v", stopErr),
				false, execution.generation)
			publishCompletion = true
		}
		if publishCompletion {
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{
				Name: execution.projectName.Name, Namespace: execution.projectName.Namespace, UID: execution.projectUID,
			}}
			stopErr = errors.Join(stopErr, execution.runtime.publish(project, execution.status.DeepCopy(), nil))
		}
		execution.statusMu.Unlock()
	}
	return stopErr
}

// launch leaves process lifetime separate from the caller's transport setup deadline.
func (runtime *projectDebugRuntime) launch(
	setupCtx context.Context, project *apiv2.Project, mode apiv2.DebugAdapterMode,
) (*projectDebugExecution, *projectDebugTransport, error) {
	select {
	case runtime.launches <- struct{}{}:
		defer func() { <-runtime.launches }()
	case <-setupCtx.Done():
		return nil, nil, setupCtx.Err()
	}
	id, idErr := randdata.MakeRandomString(16)
	if idErr != nil {
		return nil, nil, idErr
	}
	executionCtx, cancel := context.WithCancel(runtime.ctx)
	execution := &projectDebugExecution{
		runtime: runtime, cancel: cancel, done: make(chan struct{}),
		projectName: project.NamespacedName(), projectUID: project.UID, generation: project.Generation,
		status: apiv2.ProjectDebugExecutionStatus{ID: string(id), Phase: apiv2.ProjectDebugPhasePending},
	}
	if project.Spec.Container != nil {
		execution.containerID = project.Status.ContainerID
	}
	if _, publishErr := runtime.writer.update(setupCtx, project.NamespacedName(), project.UID, execution.status.DeepCopy(), nil); publishErr != nil {
		cancel()
		return nil, nil, publishErr
	}
	stopSetup := context.AfterFunc(setupCtx, cancel)
	defer stopSetup()
	if project.Spec.Container != nil {
		return runtime.launchContainer(setupCtx, executionCtx, project, execution)
	}
	var listener net.Listener
	var transport *projectDebugTransport
	var cmd *exec.Cmd
	var stdout *io.PipeWriter
	fail := func(launchErr error) (*projectDebugExecution, *projectDebugTransport, error) {
		if listener != nil {
			_ = listener.Close()
		}
		if transport != nil {
			launchErr = errors.Join(launchErr, transport.Close())
		} else if stdout != nil {
			launchErr = errors.Join(launchErr, stdout.Close())
		}
		if execution.handle.Pid > 0 {
			launchErr = errors.Join(launchErr, execution.stop())
		} else {
			cancel()
		}
		execution.statusMu.Lock()
		execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.NowMicro()
		if execution.handle.Pid > 0 && !process.IsProcessGoneErr(runtime.executor.CheckProcessRunning(execution.handle)) {
			execution.status.FinishedAt = metav1.MicroTime{}
		}
		execution.status.Conditions = debugConditions("LaunchFailed", launchErr.Error(), false, project.Generation)
		publishErr := runtime.publish(project, execution.status.DeepCopy(), nil)
		execution.statusMu.Unlock()
		if execution.handle.Pid > 0 {
			return execution, nil, errors.Join(launchErr, publishErr)
		}
		return nil, nil, errors.Join(launchErr, publishErr)
	}
	directoryInfo, directoryErr := os.Stat(project.Spec.DebugAdapter.Directory)
	if directoryErr != nil {
		return fail(fmt.Errorf("inspect adapter directory: %w", directoryErr))
	}
	if !directoryInfo.IsDir() {
		return fail(fmt.Errorf("adapter directory %q is not a directory", project.Spec.DebugAdapter.Directory))
	}
	args := project.Spec.DebugAdapter.GetArgs(false)
	if mode != apiv2.DebugAdapterModeStdio {
		port, portErr := networking.GetFreePort(commonapi.PortProtocolTCP, "127.0.0.1", runtime.log)
		if portErr != nil {
			return fail(portErr)
		}
		execution.address = net.JoinHostPort("127.0.0.1", strconv.FormatInt(int64(port), 10))
	}
	if mode == apiv2.DebugAdapterModeTCPCallback {
		var listenErr error
		listener, listenErr = net.Listen("tcp", execution.address)
		if listenErr != nil {
			return fail(listenErr)
		}
	}
	if mode != apiv2.DebugAdapterModeStdio {
		_, portText, portErr := net.SplitHostPort(execution.address)
		if portErr != nil {
			return fail(portErr)
		}
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], "{{port}}", portText)
		}
	}
	cmd = exec.Command(args[0], args[1:]...)
	cmd.Env = debugAdapterEnvironment(project.Spec.DebugAdapter)
	cmd.WaitDelay = 2 * time.Second
	diagnostic := adapterDiagnosticWriter{log: runtime.log.WithValues(
		logger.RESOURCE_LOG_STREAM_ID, project.GetResourceId(), "Project", project.NamespacedName(), "Execution", string(id))}
	cmd.Stderr = diagnostic
	if mode == apiv2.DebugAdapterModeStdio {
		stdin, stdinErr := cmd.StdinPipe()
		if stdinErr != nil {
			return fail(stdinErr)
		}
		reader, output := io.Pipe()
		stdout = output
		cmd.Stdout = stdout
		transport = &projectDebugTransport{Reader: reader, Writer: stdin, close: func() error {
			var closeErrors error
			closeErrors = errors.Join(closeErrors, stdout.Close(), reader.Close())
			// Executor admission can fail before exec.Cmd.Start takes ownership of its child-side pipe.
			for _, closer := range []io.Closer{stdin, cmd.Stdin.(io.Closer)} {
				closeErr := closer.Close()
				if !errors.Is(closeErr, os.ErrClosed) && !errors.Is(closeErr, io.ErrClosedPipe) {
					closeErrors = errors.Join(closeErrors, closeErr)
				}
			}
			return closeErrors
		}}
	} else {
		cmd.Stdout = diagnostic
	}
	onExit := process.ProcessExitHandlerFunc(func(_ process.Pid_t, code int32, exitErr error) {
		defer cancel()
		if stdout != nil {
			if closeErr := stdout.Close(); closeErr != nil {
				runtime.log.Error(closeErr, "Failed to close adapter output pipe", "Project", project.NamespacedName())
			}
		}
		execution.statusMu.Lock()
		defer execution.statusMu.Unlock()
		execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
		if exitErr == nil && code != process.UnknownExitCode {
			execution.status.ExitCode = &code
		}
		if exitErr != nil && !errors.Is(exitErr, context.Canceled) {
			if code == process.UnknownExitCode {
				execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.MicroTime{}
			}
			execution.status.Conditions = debugConditions("AdapterExited", exitErr.Error(), false, project.Generation)
		} else {
			execution.status.Conditions = debugConditions("AdapterExited", "The adapter process exited.", false, project.Generation)
		}
		close(execution.done)
		if publishErr := runtime.publish(project, execution.status.DeepCopy(), nil); publishErr != nil {
			runtime.log.Error(publishErr, "Failed to publish debug adapter completion")
		}
	})
	handle, wait, startErr := runtime.executor.StartProcess(executionCtx, cmd, onExit, process.CreationFlagEnsureKillOnDispose, nil)
	if startErr != nil {
		return fail(startErr)
	}
	execution.handle = handle
	dcpproc.RunProcessWatcher(runtime.executor, handle, runtime.log)
	pid := int64(handle.Pid)
	execution.statusMu.Lock()
	execution.status.PID, execution.status.IdentityTimestamp = &pid, metav1.NewMicroTime(handle.IdentityTime)
	if execution.status.Phase == apiv2.ProjectDebugPhasePending {
		execution.status.Phase = apiv2.ProjectDebugPhaseRunning
		execution.status.Conditions = debugConditions("AdapterRunning", "The adapter process was launched.", true, project.Generation)
	}
	_, publishErr := runtime.writer.update(setupCtx, project.NamespacedName(), project.UID, execution.status.DeepCopy(), nil)
	execution.statusMu.Unlock()
	wait()
	if publishErr != nil {
		return fail(publishErr)
	}
	if mode == apiv2.DebugAdapterModeTCPCallback {
		callbackListener := listener
		closeOnCancel := context.AfterFunc(setupCtx, func() { _ = callbackListener.Close() })
		closeOnExit := context.AfterFunc(executionCtx, func() { _ = callbackListener.Close() })
		connection, acceptErr := listener.Accept()
		closeOnCancel()
		closeOnExit()
		_ = listener.Close()
		listener = nil
		if acceptErr != nil {
			return fail(errors.Join(setupCtx.Err(), acceptErr))
		}
		transport = &projectDebugTransport{Reader: connection, Writer: connection, close: connection.Close}
	} else if mode == apiv2.DebugAdapterModeTCPConnect {
		connection, dialErr := execution.connect(setupCtx)
		if dialErr != nil {
			return fail(dialErr)
		}
		transport = &projectDebugTransport{Reader: connection, Writer: connection, close: connection.Close}
	}
	if setupErr := setupCtx.Err(); setupErr != nil {
		return fail(setupErr)
	}
	return execution, transport, nil
}

func (runtime *projectDebugRuntime) launchContainer(
	setupCtx, executionCtx context.Context, project *apiv2.Project, execution *projectDebugExecution,
) (*projectDebugExecution, *projectDebugTransport, error) {
	var transport *projectDebugTransport
	var stdout *io.PipeWriter
	started := false
	fail := func(launchErr error) (*projectDebugExecution, *projectDebugTransport, error) {
		if transport != nil {
			launchErr = errors.Join(launchErr, transport.Close())
		} else if stdout != nil {
			launchErr = errors.Join(launchErr, stdout.Close())
		}
		if started {
			launchErr = errors.Join(launchErr, execution.stop())
		} else {
			execution.cancel()
		}
		execution.statusMu.Lock()
		if !started || execution.status.Phase != apiv2.ProjectDebugPhaseExited {
			execution.status.Phase = apiv2.ProjectDebugPhaseFailed
		}
		if !started {
			execution.status.FinishedAt = metav1.NowMicro()
		}
		execution.status.Conditions = debugConditions("LaunchFailed", launchErr.Error(), false, project.Generation)
		publishErr := runtime.publish(project, execution.status.DeepCopy(), nil)
		execution.statusMu.Unlock()
		if started {
			return execution, nil, errors.Join(launchErr, publishErr)
		}
		return nil, nil, errors.Join(launchErr, publishErr)
	}
	orchestrator, runtimeErr := runtime.findContainerRuntime(setupCtx, project.Status.ContainerRuntime)
	if runtimeErr != nil {
		return fail(fmt.Errorf("resolve prepared container runtime: %w", runtimeErr))
	}
	if orchestrator == nil {
		return fail(fmt.Errorf("selected container runtime returned no exec implementation"))
	}
	var environment []containers.EnvVar
	for _, entry := range project.Spec.DebugAdapter.Env {
		environment = append(environment, containers.EnvVar{Name: entry.Name, Value: entry.Value})
	}
	diagnostic := adapterDiagnosticWriter{log: runtime.log.WithValues(
		logger.RESOURCE_LOG_STREAM_ID, project.GetResourceId(), "Project", project.NamespacedName(), "Execution", execution.status.ID)}
	args := project.Spec.DebugAdapter.GetArgs(true)
	options := containers.ExecContainerOptions{
		Container: execution.containerID, Env: environment,
		StreamCommandOptions: containers.StreamCommandOptions{StdOutStream: usvc_io.NopWriteCloser(diagnostic), StdErrStream: usvc_io.NopWriteCloser(diagnostic)},
	}
	if project.Spec.DebugAdapter.EffectiveMode() == apiv2.DebugAdapterModeStdio {
		stdin, input, pipeErr := os.Pipe()
		if pipeErr != nil {
			return fail(fmt.Errorf("create container adapter input pipe: %w", pipeErr))
		}
		reader, output := io.Pipe()
		stdout = output
		options.StdInStream, options.StdOutStream = stdin, stdout
		transport = &projectDebugTransport{Reader: reader, Writer: input, close: func() error {
			closeErrors := errors.Join(stdout.Close(), reader.Close())
			for _, closer := range []io.Closer{stdin, input} {
				closeErr := closer.Close()
				if !errors.Is(closeErr, os.ErrClosed) && !errors.Is(closeErr, io.ErrClosedPipe) {
					closeErrors = errors.Join(closeErrors, closeErr)
				}
			}
			return closeErrors
		}}
	} else {
		if project.Status.DebugAdapterPort <= 0 || project.Status.DebugAdapterAddress == "" {
			return fail(fmt.Errorf("container adapter TCP endpoint is not prepared"))
		}
		execution.address = project.Status.DebugAdapterAddress
		for index := range args {
			args[index] = strings.ReplaceAll(args[index], "{{port}}", strconv.Itoa(int(project.Status.DebugAdapterPort)))
		}
	}
	options.Command, options.Args = args[0], args[1:]
	if setupErr := setupCtx.Err(); setupErr != nil {
		return fail(setupErr)
	}
	exits, execErr := orchestrator.ExecContainer(executionCtx, options)
	if execErr != nil {
		return fail(execErr)
	}
	started = true
	if exits == nil {
		close(execution.done)
		return fail(fmt.Errorf("container adapter execution returned no completion channel"))
	}
	execution.statusMu.Lock()
	execution.status.Phase = apiv2.ProjectDebugPhaseRunning
	execution.status.Conditions = debugConditions("AdapterExecRunning", "The container adapter's attached execution was started.", true, project.Generation)
	_, publishErr := runtime.writer.update(setupCtx, project.NamespacedName(), project.UID, execution.status.DeepCopy(), nil)
	execution.statusMu.Unlock()
	go func() {
		defer execution.cancel()
		var code int32
		var open bool
		select {
		case code, open = <-exits:
		case <-executionCtx.Done():
			settlementCtx, settlementCancel := process.WithDetachedStopTimeout(executionCtx)
			select {
			case code, open = <-exits:
			case <-settlementCtx.Done():
			}
			settlementCancel()
		}
		if stdout != nil {
			if closeErr := stdout.Close(); closeErr != nil {
				runtime.log.Error(closeErr, "Failed to close container adapter output pipe", "Project", project.NamespacedName())
			}
		}
		execution.statusMu.Lock()
		if !open || code == process.UnknownExitCode || executionCtx.Err() != nil {
			execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.MicroTime{}
			execution.status.Conditions = debugConditions("ContainerAdapterCleanupUnconfirmed",
				"The attached execution ended without confirming in-container adapter termination.", false, project.Generation)
		} else {
			execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
			execution.status.ExitCode = &code
			execution.status.Conditions = debugConditions("AdapterExecExited",
				"The runtime reported completion of the container adapter command.", false, project.Generation)
		}
		close(execution.done)
		if completionErr := runtime.publish(project, execution.status.DeepCopy(), nil); completionErr != nil {
			runtime.log.Error(completionErr, "Failed to publish container adapter completion", "Project", project.NamespacedName())
		}
		execution.statusMu.Unlock()
	}()
	if publishErr != nil {
		return fail(publishErr)
	}
	if setupErr := setupCtx.Err(); setupErr != nil {
		return fail(setupErr)
	}
	if project.Spec.DebugAdapter.EffectiveMode() == apiv2.DebugAdapterModeTCPConnect {
		connection, connectErr := execution.connect(setupCtx)
		if connectErr != nil {
			return fail(connectErr)
		}
		transport = &projectDebugTransport{Reader: connection, Writer: connection, close: connection.Close}
	}
	if setupErr := setupCtx.Err(); setupErr != nil {
		return fail(setupErr)
	}
	return execution, transport, nil
}

func (execution *projectDebugExecution) stopContainer() error {
	execution.cancel()
	cleanupCtx, cancel := process.WithDetachedStopTimeout(execution.runtime.ctx)
	defer cancel()
	select {
	case <-execution.done:
	case <-cleanupCtx.Done():
	}
	execution.statusMu.Lock()
	defer execution.statusMu.Unlock()
	if execution.status.Phase == apiv2.ProjectDebugPhaseExited && !execution.status.FinishedAt.IsZero() {
		return nil
	}
	stopErr := errors.Join(cleanupCtx.Err(), fmt.Errorf("in-container adapter termination is unconfirmed"))
	execution.status.Phase, execution.status.FinishedAt = apiv2.ProjectDebugPhaseFailed, metav1.MicroTime{}
	execution.status.Conditions = debugConditions("ContainerAdapterCleanupUnconfirmed", stopErr.Error(), false, execution.generation)
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: execution.projectName.Name, Namespace: execution.projectName.Namespace, UID: execution.projectUID}}
	return errors.Join(stopErr, execution.runtime.publish(project, execution.status.DeepCopy(), nil))
}

func (execution *projectDebugExecution) connect(ctx context.Context) (io.ReadWriteCloser, error) {
	dial := func(dialCtx context.Context) (net.Conn, error) {
		select {
		case <-execution.done:
			return nil, resiliency.Permanent(fmt.Errorf("adapter exited before connection"))
		default:
		}
		return (&net.Dialer{}).DialContext(dialCtx, "tcp", execution.address)
	}
	var connection net.Conn
	dialErr := resiliency.RetryExponential(ctx, func() error {
		var connectErr error
		connection, connectErr = dial(ctx)
		return connectErr
	})
	if dialErr != nil || execution.containerID == "" {
		return connection, dialErr
	}
	// Container runtime port proxies accept connections before the in-container adapter listens,
	// so the connection remains redialable until the adapter's first output proves it reached the adapter.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return nil, errors.Join(fmt.Errorf("container adapter connection requires a deadline"), connection.Close())
	}
	redialCtx, redialCancel := context.WithDeadline(execution.runtime.ctx, deadline)
	return newRedialingAdapterConnection(redialCtx, redialCancel, dial, connection), nil
}

// redialingAdapterConnection queues client output in a bounded pipe and forwards it to the adapter connection,
// retaining forwarded output until the adapter's first output arrives. If the connection ends before then, it dials
// again and replays the retained output, as a runtime port proxy may accept and then close a connection that has
// no in-container listener behind it.
type redialingAdapterConnection struct {
	ctx    context.Context
	cancel context.CancelFunc
	dial   func(context.Context) (net.Conn, error)

	pendingReader io.ReadCloser
	pendingWriter io.WriteCloser
	forwarded     chan struct{}

	// mu guards the fields below; a redial holds it until the replacement connection has received the replay.
	mu         sync.Mutex
	connection net.Conn
	// replay holds forwarded output that must be resent if the connection ends before the adapter responds,
	// because reads from the pending pipe consume its data.
	replay []byte
	proven bool
}

func newRedialingAdapterConnection(
	ctx context.Context, cancel context.CancelFunc, dial func(context.Context) (net.Conn, error), connection net.Conn,
) *redialingAdapterConnection {
	pendingReader, pendingWriter := usvc_io.NewBufferedPipeWithMaxSize(proxy.ParkedConnectionMaxBufferSize)
	redialing := &redialingAdapterConnection{
		ctx: ctx, cancel: cancel, dial: dial,
		pendingReader: pendingReader, pendingWriter: pendingWriter, forwarded: make(chan struct{}),
		connection: connection,
	}
	go redialing.forward()
	return redialing
}

func (connection *redialingAdapterConnection) forward() {
	defer close(connection.forwarded)
	chunk := make([]byte, 32*1024)
	for {
		count, readErr := connection.pendingReader.Read(chunk)
		if count > 0 {
			connection.mu.Lock()
			current, proven := connection.connection, connection.proven
			if !proven {
				if len(connection.replay)+count > proxy.ParkedConnectionMaxBufferSize {
					connection.mu.Unlock()
					overflowErr := fmt.Errorf("container adapter connection buffered more than %d bytes before the adapter responded", proxy.ParkedConnectionMaxBufferSize)
					_ = connection.pendingReader.(*usvc_io.BufferedPipeReader).CloseWithError(overflowErr)
					_ = current.Close()
					return
				}
				connection.replay = append(connection.replay, chunk[:count]...)
			}
			connection.mu.Unlock()
			// A failed write before the adapter responds is replayed by the redial that the ended read triggers.
			if _, writeErr := current.Write(chunk[:count]); writeErr != nil && proven {
				_ = connection.pendingReader.(*usvc_io.BufferedPipeReader).CloseWithError(writeErr)
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (connection *redialingAdapterConnection) Read(payload []byte) (int, error) {
	for {
		connection.mu.Lock()
		current, proven := connection.connection, connection.proven
		connection.mu.Unlock()
		count, readErr := current.Read(payload)
		if count > 0 && !proven {
			connection.mu.Lock()
			connection.proven, connection.replay = true, nil
			connection.mu.Unlock()
		}
		if count > 0 || proven || readErr == nil {
			return count, readErr
		}
		if connection.ctx.Err() != nil {
			return 0, readErr
		}
		if redialErr := connection.redial(current); redialErr != nil {
			return 0, errors.Join(readErr, redialErr)
		}
	}
}

func (connection *redialingAdapterConnection) redial(stale net.Conn) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	_ = stale.Close()
	var replacement net.Conn
	redialErr := resiliency.RetryExponential(connection.ctx, func() error {
		candidate, dialErr := connection.dial(connection.ctx)
		if dialErr != nil {
			return dialErr
		}
		if _, writeErr := candidate.Write(connection.replay); writeErr != nil {
			return errors.Join(writeErr, candidate.Close())
		}
		replacement = candidate
		return nil
	})
	if redialErr != nil {
		return fmt.Errorf("container adapter connection closed before the adapter responded: %w", redialErr)
	}
	connection.connection = replacement
	return nil
}

func (connection *redialingAdapterConnection) Write(payload []byte) (int, error) {
	return connection.pendingWriter.Write(payload)
}

func (connection *redialingAdapterConnection) Close() error {
	connection.cancel()
	_ = connection.pendingWriter.Close()
	_ = connection.pendingReader.Close()
	connection.mu.Lock()
	current := connection.connection
	connection.mu.Unlock()
	closeErr := current.Close()
	<-connection.forwarded
	return closeErr
}

func (runtime *projectDebugRuntime) acquire(
	ctx context.Context, project *apiv2.Project,
) (*projectDebugExecution, *projectDebugTransport, func() error, error) {
	if project.Spec.DebugAdapter.EffectiveMode() != apiv2.DebugAdapterModeTCPConnect {
		execution, transport, launchErr := runtime.launch(ctx, project, project.Spec.DebugAdapter.EffectiveMode())
		if launchErr != nil {
			return nil, nil, nil, launchErr
		}
		return execution, transport, execution.stop, nil
	}
	for {
		runtime.mu.Lock()
		provider := runtime.providers[project.UID]
		if provider != nil && provider.refs == 0 && (provider.fenced && provider.execution.containerID != "" || !provider.stopping && provider.retained()) &&
			(provider.execution.containerID != project.Status.ContainerID || provider.execution.address != project.Status.DebugAdapterAddress) {
			// A retained adapter belongs to a superseded container endpoint, so it cannot collide with a new launch.
			runtime.forget(project.UID, provider)
			provider.execution.cancel()
			provider = nil
		}
		if provider != nil && provider.stopping {
			runtime.mu.Unlock()
			select {
			case <-provider.stopped:
				continue
			case <-ctx.Done():
				return nil, nil, nil, ctx.Err()
			}
		}
		first := provider == nil
		if first {
			startCtx, startCancel := context.WithTimeout(runtime.ctx, project.Spec.DebugAdapter.GetConnectionTimeout())
			provider = &projectDebugProvider{ready: make(chan struct{}), stopped: make(chan struct{}), cancel: startCancel}
			runtime.providers[project.UID] = provider
			go func(active *projectDebugProvider) {
				active.execution, active.initial, active.err = runtime.launch(startCtx, project, apiv2.DebugAdapterModeTCPConnect)
				close(active.ready)
				startCancel()
				if active.err == nil && active.execution.containerID != "" {
					go runtime.forgetOnExit(project.UID, active)
				}
			}(provider)
		}
		provider.refs++
		runtime.mu.Unlock()
		release := func() error { return runtime.release(project.UID, provider) }
		select {
		case <-provider.ready:
		case <-ctx.Done():
			return nil, nil, nil, errors.Join(ctx.Err(), release())
		}
		if provider.err != nil {
			return nil, nil, nil, errors.Join(provider.err, release())
		}
		if provider.execution.containerID != "" &&
			(provider.execution.containerID != project.Status.ContainerID || provider.execution.address != project.Status.DebugAdapterAddress) {
			return nil, nil, nil, errors.Join(fmt.Errorf("prepared container adapter endpoint changed"), release())
		}
		runtime.mu.Lock()
		initial := provider.initial
		provider.initial = nil
		runtime.mu.Unlock()
		if initial != nil {
			return provider.execution, initial, release, nil
		}
		connection, connectErr := provider.execution.connect(ctx)
		if connectErr != nil {
			return nil, nil, nil, errors.Join(connectErr, release())
		}
		return provider.execution, &projectDebugTransport{Reader: connection, Writer: connection, close: connection.Close}, release, nil
	}
}

func (runtime *projectDebugRuntime) release(uid types.UID, provider *projectDebugProvider) error {
	runtime.mu.Lock()
	provider.refs--
	if provider.refs != 0 {
		runtime.mu.Unlock()
		return nil
	}
	if provider.retained() {
		// Container runtimes cannot terminate an exec'd process, so the adapter stays available until it exits on its own.
		initial := provider.initial
		provider.initial = nil
		runtime.mu.Unlock()
		if initial != nil {
			return initial.Close()
		}
		return nil
	}
	provider.stopping = true
	provider.cancel()
	runtime.mu.Unlock()
	<-provider.ready
	var stopErr error
	if provider.initial != nil {
		stopErr = provider.initial.Close()
	}
	if provider.execution != nil {
		stopErr = errors.Join(stopErr, provider.execution.stop())
		confirmed := false
		if provider.execution.containerID != "" {
			provider.execution.statusMu.Lock()
			confirmed = provider.execution.status.Phase == apiv2.ProjectDebugPhaseExited && !provider.execution.status.FinishedAt.IsZero()
			provider.execution.statusMu.Unlock()
		} else {
			confirmed = process.IsProcessGoneErr(runtime.executor.CheckProcessRunning(provider.execution.handle))
		}
		if !confirmed {
			// Keep the provider fenced when termination is unconfirmed; a new request must not launch a duplicate.
			runtime.mu.Lock()
			provider.fenced = true
			runtime.mu.Unlock()
			return errors.Join(stopErr, fmt.Errorf("shared adapter provider termination is unconfirmed"))
		}
	}
	runtime.mu.Lock()
	runtime.forget(uid, provider)
	runtime.mu.Unlock()
	return stopErr
}

// retained reports whether an idle provider is a still-running container adapter; callers must hold runtime.mu.
func (provider *projectDebugProvider) retained() bool {
	select {
	case <-provider.ready:
	default:
		return false
	}
	if provider.err != nil || provider.execution.containerID == "" {
		return false
	}
	select {
	case <-provider.execution.done:
		return false
	default:
		return true
	}
}

// forget removes a provider and unblocks waiting acquisitions; callers must hold runtime.mu.
func (runtime *projectDebugRuntime) forget(uid types.UID, provider *projectDebugProvider) {
	provider.stopping = true
	if runtime.providers[uid] == provider {
		delete(runtime.providers, uid)
	}
	close(provider.stopped)
}

// forgetOnExit removes an idle container provider once its adapter completes on its own.
func (runtime *projectDebugRuntime) forgetOnExit(uid types.UID, provider *projectDebugProvider) {
	<-provider.execution.done
	provider.execution.statusMu.Lock()
	confirmed := provider.execution.status.Phase == apiv2.ProjectDebugPhaseExited && !provider.execution.status.FinishedAt.IsZero()
	provider.execution.statusMu.Unlock()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if provider.refs != 0 || provider.stopping {
		return
	}
	if confirmed {
		runtime.forget(uid, provider)
	} else {
		// Unconfirmed termination keeps the provider fenced, matching final release of an active provider.
		provider.stopping, provider.fenced = true, true
	}
}

func debugConditions(reason, message string, ready bool, generation int64) []metav1.Condition {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return []metav1.Condition{{Type: string(apiv2.ConditionReady), Status: status, Reason: reason,
		Message: message, ObservedGeneration: generation, LastTransitionTime: metav1.Now()}}
}

func debugAdapterEnvironment(config *apiv2.DebugAdapterConfig) []string {
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "DCP_") && !strings.HasPrefix(strings.ToUpper(name), "DEBUG_SESSION_") {
			environment = append(environment, entry)
		}
	}
	for _, entry := range config.Env {
		environment = append(environment, entry.Name+"="+entry.Value)
	}
	return environment
}
