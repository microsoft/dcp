/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/process"
)

type stopProcessTreeFunc func(context.Context, process.Executor, process.ProcessHandle, logr.Logger) error

type contextualProcessExitHandler struct {
	ctx   context.Context
	inner *process.ConcurrentProcessExitHandler
}

func newContextualProcessExitHandler(ctx context.Context) *contextualProcessExitHandler {
	return &contextualProcessExitHandler{
		ctx:   ctx,
		inner: process.NewConcurrentProcessExitHandler(),
	}
}

func (handler *contextualProcessExitHandler) Exited() <-chan struct{} {
	return handler.inner.Exited()
}

func (handler *contextualProcessExitHandler) ExitInfo() process.ProcessExitInfo {
	return handler.inner.ExitInfo()
}

func (handler *contextualProcessExitHandler) OnProcessExited(pid process.Pid_t, exitCode int32, err error) {
	handler.inner.OnProcessExited(pid, exitCode, errors.Join(context.Cause(handler.ctx), err))
}

type startedWslcProcess struct {
	exitHandler *contextualProcessExitHandler
	stopResult  <-chan error
}

type wslcProcessResult struct {
	exitInfo process.ProcessExitInfo
	stopErr  error
}

type nonClosingWriter struct {
	io.Writer
}

func (started *startedWslcProcess) Exited() <-chan struct{} {
	return started.exitHandler.Exited()
}

func (started *startedWslcProcess) ExitInfo() process.ProcessExitInfo {
	return started.exitHandler.ExitInfo()
}

func (started *startedWslcProcess) stopError() error {
	return <-started.stopResult
}

func (started *startedWslcProcess) wait() wslcProcessResult {
	<-started.Exited()
	return wslcProcessResult{
		exitInfo: started.ExitInfo(),
		stopErr:  started.stopError(),
	}
}

func (result wslcProcessResult) err() error {
	return errors.Join(result.exitInfo.Err, result.stopErr)
}

func preserveWriterOwnership(writer io.Writer) io.Writer {
	if writer == nil {
		return nil
	}
	return nonClosingWriter{Writer: writer}
}

func hasNonCancellationError(err error) bool {
	if err == nil {
		return false
	}

	if joinedErr, ok := err.(interface{ Unwrap() []error }); ok {
		for _, innerErr := range joinedErr.Unwrap() {
			if hasNonCancellationError(innerErr) {
				return true
			}
		}
		return false
	}

	if innerErr := errors.Unwrap(err); innerErr != nil {
		return hasNonCancellationError(innerErr)
	}

	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

var (
	containerNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)container\s+['"].+['"]\s+not found`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")),
	)
	imageNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)image\s+['"].+['"]\s+not found`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("image not found")),
	)
	networkNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)network not found:\s*['"].+['"]`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")),
	)
	volumeNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)volume not found:\s*['"].+['"]`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")),
	)
	alreadyExistsMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(already exists|already in use|already connected|already attached)`),
		containers.ErrAlreadyExists,
	)
	objectInUseMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(is in use|being used|active endpoints|is running|running container)`),
		containers.ErrObjectInUse,
	)
	allocationFailureMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(address pool|could not allocate|no available subnet|port is already allocated|address already in use|failed to bind)`),
		containers.ErrCouldNotAllocate,
	)
	runtimeUnavailableMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(
			`(?i)(`+
				`no active (?:wslc )?(?:default )?session|`+
				`(?:session manager|default session|wslc control endpoint|session control endpoint).*`+
				`(?:not running|unavailable|not found|failed to connect|connection refused)|`+
				`(?:failed to connect|connection refused).*`+
				`(?:session manager|default session|wslc control endpoint|session control endpoint)`+
				`)`,
		),
		containers.ErrRuntimeNotHealthy,
	)
)

func makeWslcCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("wslc", args...)
	if cmd.Path != "" {
		cmd.Args[0] = cmd.Path
	}
	configureWslcCommand(cmd)
	return cmd
}

func (wco *WslcCliOrchestrator) runBufferedWslcCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdoutCloser io.WriteCloser,
	stderrCloser io.WriteCloser,
	timeout time.Duration,
) (*bytes.Buffer, *bytes.Buffer, error) {
	return wco.runBufferedWslcCommandInternal(
		ctx,
		commandName,
		cmd,
		stdoutCloser,
		stderrCloser,
		timeout,
		true,
	)
}

func (wco *WslcCliOrchestrator) runBufferedWslcCommandInternal(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdoutCloser io.WriteCloser,
	stderrCloser io.WriteCloser,
	timeout time.Duration,
	closeStreams bool,
) (*bytes.Buffer, *bytes.Buffer, error) {
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("timeout for WSLC command %q must be positive", commandName)
	}

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdoutBuffer := new(bytes.Buffer)
	if stdoutCloser == nil {
		cmd.Stdout = stdoutBuffer
	} else {
		if closeStreams {
			defer func() {
				_ = stdoutCloser.Close()
			}()
		}
		cmd.Stdout = io.MultiWriter(stdoutCloser, stdoutBuffer)
	}

	stderrBuffer := new(bytes.Buffer)
	if stderrCloser == nil {
		cmd.Stderr = stderrBuffer
	} else {
		if closeStreams {
			defer func() {
				_ = stderrCloser.Close()
			}()
		}
		cmd.Stderr = io.MultiWriter(stderrCloser, stderrBuffer)
	}
	if contextErr := effectiveCtx.Err(); contextErr != nil {
		return stdoutBuffer, stderrBuffer, contextErr
	}

	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	startedProcess, startErr := wco.startWslcProcess(
		effectiveCtx,
		commandName,
		cmd,
		process.CreationFlagsNone,
	)
	if startErr != nil {
		return stdoutBuffer, stderrBuffer, fmt.Errorf("failed to start WSLC command %q: %w", commandName, startErr)
	}

	processResult := startedProcess.wait()
	commandErr := processResult.err()
	if processResult.exitInfo.ExitCode != 0 {
		commandErr = errors.Join(
			commandErr,
			fmt.Errorf("wslc command %q returned non-zero exit code %d", commandName, processResult.exitInfo.ExitCode),
		)
	}

	return stdoutBuffer, stderrBuffer, commandErr
}

func (wco *WslcCliOrchestrator) startStreamingWslcCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdout io.Writer,
	stderr io.Writer,
) (*startedWslcProcess, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}

	cmd.Stdout = preserveWriterOwnership(stdout)
	cmd.Stderr = preserveWriterOwnership(stderr)

	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	startedProcess, startErr := wco.startWslcProcess(
		ctx,
		commandName,
		cmd,
		process.CreationFlagEnsureKillOnDispose,
	)
	if startErr != nil {
		return nil, fmt.Errorf("failed to start WSLC command %q: %w", commandName, startErr)
	}
	return startedProcess, nil
}

func (wco *WslcCliOrchestrator) startWslcProcess(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	creationFlags process.ProcessCreationFlag,
) (*startedWslcProcess, error) {
	// Generic executor cancellation cannot signal a separate Windows console. Keep cancellation
	// ownership here so dcpproc can attach to that console and stop the verified process tree.
	processCtx, processCancel := context.WithCancel(context.WithoutCancel(ctx))
	exitHandler := newContextualProcessExitHandler(ctx)
	handle, startWaitForExit, startErr := wco.executor.StartProcess(
		processCtx,
		cmd,
		exitHandler,
		creationFlags,
		nil,
	)
	if startErr != nil {
		processCancel()
		return nil, startErr
	}
	startWaitForExit()

	stopResult := make(chan error, 1)
	go func() {
		defer processCancel()
		defer close(stopResult)

		select {
		case <-exitHandler.Exited():
			stopResult <- nil
			return
		case <-ctx.Done():
		}

		stopErr := wco.stopCancelledWslcProcess(ctx, commandName, handle)
		if stopErr != nil {
			wco.log.Error(stopErr, "Could not stop cancelled WSLC command", "Command", commandName, "PID", handle.Pid)
		}
		stopResult <- stopErr
	}()

	return &startedWslcProcess{
		exitHandler: exitHandler,
		stopResult:  stopResult,
	}, nil
}

func (wco *WslcCliOrchestrator) stopCancelledWslcProcess(
	ctx context.Context,
	commandName string,
	handle process.ProcessHandle,
) error {
	stopParentCtx := context.WithoutCancel(ctx)
	stopErr := wco.stopProcessTree(stopParentCtx, wco.executor, handle, wco.log)
	if stopErr == nil || process.IsProcessGoneErr(stopErr) {
		return nil
	}

	wrappedStopErr := fmt.Errorf("stop cancelled WSLC command %q through dcpproc: %w", commandName, stopErr)

	fallbackCtx, fallbackCancel := process.WithDetachedStopTimeout(ctx)
	defer fallbackCancel()
	fallbackErr := wco.executor.StopProcess(fallbackCtx, handle)
	if process.IsProcessGoneErr(fallbackErr) {
		fallbackErr = nil
	}
	return errors.Join(wrappedStopErr, fallbackErr)
}

func normalizeCliErrors(errBuf *bytes.Buffer, extraMatches ...containers.ErrorMatch) error {
	matches := append(extraMatches, runtimeUnavailableMatch)
	return containers.NormalizeCliErrors(errBuf, matches...)
}

func appendLabelArgs(
	args []string,
	labels []containers.Label,
	objectKind string,
) ([]string, error) {
	labelValues := maps.SliceToMap(labels, func(label containers.Label) (string, string) {
		return label.Key, label.Value
	})
	labelKeys := make([]string, 0, len(labelValues))
	for key := range labelValues {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)

	for _, key := range labelKeys {
		if key == "" {
			return nil, fmt.Errorf("%s label key cannot be empty", objectKind)
		}
		args = append(args, "--label", key+"="+labelValues[key])
	}
	return args, nil
}

func parseSingleIdentifier(buffer *bytes.Buffer) (string, error) {
	if buffer == nil {
		return "", fmt.Errorf("wslc command did not return an object identifier")
	}

	lines := nonEmptyLines(buffer.Bytes())
	if len(lines) != 1 {
		return "", fmt.Errorf("wslc command output did not contain exactly one identifier: %q", buffer.String())
	}
	if strings.ContainsAny(lines[0], " \t") {
		return "", fmt.Errorf("wslc command returned an invalid identifier %q", lines[0])
	}
	return lines[0], nil
}

func nonEmptyLines(data []byte) []string {
	rawLines := bytes.Split(data, []byte{'\n'})
	lines := make([]string, 0, len(rawLines))
	for _, rawLine := range rawLines {
		line := strings.TrimSpace(string(rawLine))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func incompleteError(objectKind string, actual int, expected int) error {
	if actual >= expected {
		return nil
	}
	return errors.Join(
		containers.ErrIncomplete,
		fmt.Errorf("only %d out of %d %s were successfully processed", actual, expected, objectKind),
	)
}

func isBenignListInspectionRace(err error) bool {
	if err == nil || !errors.Is(err, containers.ErrNotFound) {
		return false
	}

	unexpectedErrors := []error{
		context.Canceled,
		context.DeadlineExceeded,
		containers.ErrUnmatched,
		containers.ErrUnmarshalling,
		containers.ErrRuntimeNotHealthy,
		containers.ErrAlreadyExists,
		containers.ErrCouldNotAllocate,
		containers.ErrObjectInUse,
	}
	for _, unexpectedErr := range unexpectedErrors {
		if errors.Is(err, unexpectedErr) {
			return false
		}
	}
	return true
}

func closeWriteCloser(closer io.WriteCloser) {
	if closer != nil {
		_ = closer.Close()
	}
}
