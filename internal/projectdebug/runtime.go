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
		if execution.status.FinishedAt.IsZero() {
			execution.status.FinishedAt = metav1.NowMicro()
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
	if project.Spec.DebugAdapter.EffectivePlacement() == apiv2.DebugAdapterPlacementContainer {
		execution.containerID = project.Status.ContainerID
	}
	if _, publishErr := runtime.writer.update(setupCtx, project.NamespacedName(), project.UID, execution.status.DeepCopy(), nil); publishErr != nil {
		cancel()
		return nil, nil, publishErr
	}
	stopSetup := context.AfterFunc(setupCtx, cancel)
	defer stopSetup()
	if project.Spec.DebugAdapter.EffectivePlacement() == apiv2.DebugAdapterPlacementContainer {
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
	args := append([]string(nil), project.Spec.DebugAdapter.Args...)
	if mode == apiv2.DebugAdapterModeTCPCallback {
		var listenErr error
		listener, listenErr = net.Listen("tcp", "127.0.0.1:0")
		if listenErr != nil {
			return fail(listenErr)
		}
		execution.address = listener.Addr().String()
	} else if mode == apiv2.DebugAdapterModeTCPConnect {
		port, portErr := networking.GetFreePort(commonapi.PortProtocolTCP, "127.0.0.1", runtime.log)
		if portErr != nil {
			return fail(portErr)
		}
		execution.address = net.JoinHostPort("127.0.0.1", strconv.FormatInt(int64(port), 10))
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
	args := append([]string(nil), project.Spec.DebugAdapter.Args...)
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

func (execution *projectDebugExecution) connect(ctx context.Context) (net.Conn, error) {
	var connection net.Conn
	dialErr := resiliency.RetryExponential(ctx, func() error {
		select {
		case <-execution.done:
			return resiliency.Permanent(fmt.Errorf("adapter exited before connection"))
		default:
		}
		var connectErr error
		connection, connectErr = (&net.Dialer{}).DialContext(ctx, "tcp", execution.address)
		return connectErr
	})
	return connection, dialErr
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
			return errors.Join(stopErr, fmt.Errorf("shared adapter provider termination is unconfirmed"))
		}
	}
	runtime.mu.Lock()
	delete(runtime.providers, uid)
	close(provider.stopped)
	runtime.mu.Unlock()
	return stopErr
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
