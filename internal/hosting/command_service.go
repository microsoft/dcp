/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hosting

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
)

type CommandServiceRunOptions uint32

const (
	// Forwards the command stderr, line by line, to the stderr of the current process.
	CommandServiceRunOptionShowStderr CommandServiceRunOptions = 0x1

	// Lets the command outlive the service context; Run returns when the context is cancelled.
	CommandServiceRunOptionDontTerminate CommandServiceRunOptions = 0x2

	// Forwards the command stdout, line by line, to the stdout of the current process.
	CommandServiceRunOptionShowStdout CommandServiceRunOptions = 0x4
)

// CommandService is a Service that runs an external command.
type CommandService struct {
	name     string
	cmd      *exec.Cmd
	executor process.Executor
	exitCode int32
	options  CommandServiceRunOptions
	log      logr.Logger

	// Destinations for forwarded command output.
	stdout io.Writer
	stderr io.Writer
}

func NewCommandService(name string, cmd *exec.Cmd, executor process.Executor, options CommandServiceRunOptions, log logr.Logger) *CommandService {
	svc := CommandService{
		name:     name,
		cmd:      cmd,
		exitCode: process.UnknownExitCode,
		options:  options,
		log:      log,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}

	if executor == nil {
		svc.executor = process.NewOSExecutor(log)
	} else {
		svc.executor = executor
	}

	return &svc
}

func (s *CommandService) Name() string {
	return s.name
}

func (s *CommandService) Run(ctx context.Context) error {
	// Forwarding continues until the command exits, even if Run returns earlier (CommandServiceRunOptionDontTerminate),
	// so that the command output is not lost and the command never writes to a closed pipe.
	var outputWriters []io.Closer
	if (s.options & CommandServiceRunOptionShowStderr) != 0 {
		outputWriters = append(outputWriters, s.forwardOutput(&s.cmd.Stderr, s.stderr, "stderr"))
	}
	if (s.options & CommandServiceRunOptionShowStdout) != 0 {
		outputWriters = append(outputWriters, s.forwardOutput(&s.cmd.Stdout, s.stdout, "stdout"))
	}
	closeOutputWriters := func() {
		for _, w := range outputWriters {
			_ = w.Close()
		}
	}

	runCtx := ctx
	if (s.options & CommandServiceRunOptionDontTerminate) != 0 {
		runCtx = context.Background()
	}

	pic := make(chan process.ProcessExitInfo, 1)
	channelExitHandler := process.NewChannelProcessExitHandler(pic)
	peh := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		// The executor reports the exit after all command output has been copied to the output writers.
		closeOutputWriters()
		channelExitHandler.OnProcessExited(pid, exitCode, err)
	})

	_, startWaitForProcessExit, startErr := s.executor.StartProcess(runCtx, s.cmd, peh, process.CreationFlagsNone, nil)
	if startErr != nil {
		closeOutputWriters()
		return startErr
	}

	startWaitForProcessExit()

	if (s.options & CommandServiceRunOptionDontTerminate) != 0 {
		select {
		case exitInfo := <-pic:
			return s.recordExit(ctx, exitInfo)
		case <-ctx.Done():
			return nil
		}
	} else {
		// Context cancel will force the process to exit, so we only need to wait for the latter
		// (and capture the result).
		exitInfo := <-pic
		return s.recordExit(ctx, exitInfo)
	}
}

// forwardOutput makes the command write the given output stream to a new pipe and starts forwarding
// the pipe contents to dst. The returned writer must be closed when the command exits.
func (s *CommandService) forwardOutput(cmdOutput *io.Writer, dst io.Writer, streamName string) io.Closer {
	reader, writer := usvc_io.NewBufferedPipe()
	*cmdOutput = writer
	go func() {
		defer func() { _ = reader.Close() }()
		forwardLines(reader, dst, streamName, s.log.WithValues("Service", s.name))
	}()
	return writer
}

// recordExit captures the process exit code and reports an unexpected exit as a service failure.
// An exit that follows cancellation of the service context is expected and is not an error.
func (s *CommandService) recordExit(ctx context.Context, exitInfo process.ProcessExitInfo) error {
	s.exitCode = exitInfo.ExitCode
	if exitInfo.Err != nil {
		return exitInfo.Err
	}
	if exitInfo.ExitCode != 0 && ctx.Err() == nil {
		return fmt.Errorf("%s exited unexpectedly with exit code %d", s.name, exitInfo.ExitCode)
	}
	return nil
}

var _ Service = (*CommandService)(nil)
