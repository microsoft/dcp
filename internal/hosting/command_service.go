package hosting

import (
	"context"
	"os/exec"

	"github.com/usvc-dev/stdtypes/pkg/process"
)

// CommandService is a Service that runs an external command.
type CommandService struct {
	name     string
	cmd      *exec.Cmd
	executor process.Executor
	exitCode int32
}

func NewCommandService(name string, cmd *exec.Cmd, executor process.Executor) *CommandService {
	svc := CommandService{
		name:     name,
		cmd:      cmd,
		exitCode: process.UnknownExitCode,
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
	code, err := process.Run(ctx, s.executor, s.cmd)
	s.exitCode = code
	return err
}

var _ Service = (*CommandService)(nil)
