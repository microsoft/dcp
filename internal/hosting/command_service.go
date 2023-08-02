package hosting

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type CommandServiceRunOptions uint32

const (
	CommandServiceRunOptionShowStderr CommandServiceRunOptions = 0x1
)

// CommandService is a Service that runs an external command.
type CommandService struct {
	name     string
	cmd      *exec.Cmd
	executor process.Executor
	exitCode int32
	options  CommandServiceRunOptions
}

func NewCommandService(name string, cmd *exec.Cmd, executor process.Executor, options CommandServiceRunOptions) *CommandService {
	svc := CommandService{
		name:     name,
		cmd:      cmd,
		exitCode: process.UnknownExitCode,
		options:  options,
	}

	if executor == nil {
		svc.executor = process.NewOSExecutor()
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
		reader, writer := io.NewBufferedPipe()
		s.cmd.Stderr = writer
		defer writer.Close() // Ensure the following goroutine exits
		go func() {
			scanner := bufio.NewScanner(reader)
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Fprintln(os.Stderr, line)
			}
		}()
	}

	code, err := process.Run(ctx, s.executor, s.cmd)
	s.exitCode = code
	return err
}

var _ Service = (*CommandService)(nil)
