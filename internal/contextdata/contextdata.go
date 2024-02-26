// Copyright (c) Microsoft Corporation. All rights reserved.

package contextdata

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type DcpContextValueKey string

const (
	HostLifetimeContextKey    DcpContextValueKey = "HostLifetimeContext"
	LoggerContextKey          DcpContextValueKey = "LoggerContext"
	ProcessExecutorContextKey DcpContextValueKey = "ProcessExecutor"
)

func GetContextLogger(ctx context.Context) logr.Logger {
	if v := ctx.Value(LoggerContextKey); v != nil {
		if l, ok := v.(logr.Logger); ok {
			return l
		}
	}
	return logr.New(nil)
}

// Returns the host lifetime context stored in the passed context.
// This is useful for goroutines serve long-running operations that survive the lifetime of the request.
func GetHostLifetimeContext(ctx context.Context) context.Context {
	if v := ctx.Value(HostLifetimeContextKey); v != nil {
		if l, ok := v.(context.Context); ok {
			return l
		}
	}

	// If this function is called, we expect the lifetime context to be part of the passed (API server request) context,
	// so we should not really end up here. But if we do, we prefer to err on the side of returning prematurely
	// rather than allowing something to go on forever. So no context.Background() as a default.
	return ctx
}

func GetProcessExecutor(ctx context.Context) process.Executor {
	if v := ctx.Value(ProcessExecutorContextKey); v != nil {
		if l, ok := v.(process.Executor); ok {
			return l
		}
	}
	return &dummyProcessExecutor{}
}

type dummyProcessExecutor struct{}

func (*dummyProcessExecutor) StartProcess(ctx context.Context, cmd *exec.Cmd, exitHandler process.ProcessExitHandler) (pid process.Pid_t, startWaitForProcessExit func(), err error) {
	return process.UnknownPID, nil, fmt.Errorf("there is no process executor configured, no processes can be started")
}

func (*dummyProcessExecutor) StopProcess(pid process.Pid_t) error {
	return fmt.Errorf("there is no process executor configured, no processes can be stopped")
}

var _ process.Executor = (*dummyProcessExecutor)(nil)
