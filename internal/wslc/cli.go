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

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/process"
)

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
	return cmd
}

func (wco *WslcCliOrchestrator) MakeCommand(args ...string) *exec.Cmd {
	return makeWslcCommand(args...)
}

func (wco *WslcCliOrchestrator) RunBufferedCommand(
	ctx context.Context,
	opName string,
	cmd *exec.Cmd,
	stdout io.WriteCloser,
	stderr io.WriteCloser,
	timeout time.Duration,
) (*bytes.Buffer, *bytes.Buffer, error) {
	return wco.runBufferedWslcCommand(ctx, opName, cmd, stdout, stderr, timeout)
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

	exitHandler := process.NewConcurrentProcessExitHandler()
	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	_, startWaitForExit, startErr := wco.executor.StartProcess(
		effectiveCtx,
		cmd,
		exitHandler,
		process.CreationFlagsNone,
		nil,
	)
	if startErr != nil {
		return stdoutBuffer, stderrBuffer, fmt.Errorf("failed to start WSLC command %q: %w", commandName, startErr)
	}
	startWaitForExit()

	<-exitHandler.Exited()
	exitInfo := exitHandler.ExitInfo()
	var commandErr error
	if exitInfo.Err != nil {
		commandErr = exitInfo.Err
	}
	if exitInfo.ExitCode != 0 {
		commandErr = errors.Join(
			commandErr,
			fmt.Errorf("wslc command %q returned non-zero exit code %d", commandName, exitInfo.ExitCode),
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
) (*process.ConcurrentProcessExitHandler, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	exitHandler := process.NewConcurrentProcessExitHandler()
	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	_, startWaitForExit, startErr := wco.executor.StartProcess(
		ctx,
		cmd,
		exitHandler,
		process.CreationFlagEnsureKillOnDispose,
		nil,
	)
	if startErr != nil {
		return nil, fmt.Errorf("failed to start WSLC command %q: %w", commandName, startErr)
	}
	startWaitForExit()
	return exitHandler, nil
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

func closeWriteCloser(closer io.WriteCloser) {
	if closer != nil {
		_ = closer.Close()
	}
}

var _ containers.CLICommandRunner = (*WslcCliOrchestrator)(nil)
