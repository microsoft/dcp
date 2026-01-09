// Copyright (c) Microsoft Corporation. All rights reserved.

package hosting

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
)

type CommandServiceRunOptions uint32

const (
	CommandServiceRunOptionShowStderr    CommandServiceRunOptions = 0x1
	CommandServiceRunOptionDontTerminate CommandServiceRunOptions = 0x2
)

// CommandService is a Service that runs an external command.
type CommandService struct {
	name     string
	cmd      *exec.Cmd
	executor process.Executor
	exitCode int32
	options  CommandServiceRunOptions
}

func NewCommandService(name string, cmd *exec.Cmd, executor process.Executor, options CommandServiceRunOptions, log logr.Logger) *CommandService {
	svc := CommandService{
		name:     name,
		cmd:      cmd,
		exitCode: process.UnknownExitCode,
		options:  options,
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
	if (s.options & CommandServiceRunOptionShowStderr) != 0 {
		reader, writer := usvc_io.NewBufferedPipe()
		s.cmd.Stderr = writer
		defer writer.Close() // Ensure the following goroutine exits

		go func() {
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				// Do not check for- and bail out if the context is cancelled here--we want to capture all output
				// from the command. The scanner will end automatically when the process ends and the writer is closed.
				line := scanner.Text()
				fmt.Fprintln(os.Stderr, line)
			}
		}()
	}

	runCtx := ctx
	if (s.options & CommandServiceRunOptionDontTerminate) != 0 {
		runCtx = context.Background()
	}

	pic := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pic)

	_, _, startWaitForProcessExit, startErr := s.executor.StartProcess(runCtx, s.cmd, peh, process.CreationFlagsNone)
	if startErr != nil {
		return startErr
	}

	startWaitForProcessExit()

	if (s.options & CommandServiceRunOptionDontTerminate) != 0 {
		select {
		case exitInfo := <-pic:
			s.exitCode = exitInfo.ExitCode
			return exitInfo.Err
		case <-ctx.Done():
			return nil
		}
	} else {
		// Context cancel will force the process to exit, so we only need to wait for the latter
		// (and capture the result).
		exitInfo := <-pic
		s.exitCode = exitInfo.ExitCode
		return exitInfo.Err
	}
}

var _ Service = (*CommandService)(nil)
