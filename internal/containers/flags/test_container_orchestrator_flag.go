package flags

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

const TestContainerOrchestratorSocketFlagName = "test-container-orchestrator-socket"

type TestContainerOrchestratorSocketFlagValue struct {
	SocketFilePath string
}

var testContainerOrchestratorSocketFlagValue *TestContainerOrchestratorSocketFlagValue = nil

func EnsureTestContainerOrchestratorSocketFlag(flags *pflag.FlagSet) {
	flags.Var(testContainerOrchestratorSocketFlagValue, TestContainerOrchestratorSocketFlagName, "Used to specify the path of the Unix domain socket file for the test container orchestrator.")
	_ = flags.MarkHidden(TestContainerOrchestratorSocketFlagName)
}

func (sfv *TestContainerOrchestratorSocketFlagValue) Set(val string) error {
	// If the value does not point to a valid file, that is an error
	file, err := os.Stat(val)
	if err != nil {
		return fmt.Errorf("invalid test container orchestrator socket file path: %s", err)
	}
	if file.IsDir() {
		return fmt.Errorf("test container orchestrator socket path '%s' is a directory", val)
	}

	testContainerOrchestratorSocketFlagValue = &TestContainerOrchestratorSocketFlagValue{
		SocketFilePath: val,
	}
	return nil
}

func (sfv *TestContainerOrchestratorSocketFlagValue) String() string {
	if sfv == nil {
		return ""
	} else {
		return sfv.SocketFilePath
	}
}

func (sfv *TestContainerOrchestratorSocketFlagValue) Type() string {
	return TestContainerOrchestratorSocketFlagName
}

func TryGetRemoteContainerOrchestrator(lifetimeCtx context.Context, log logr.Logger) containers.RemoteContainerOrchestrator {
	if testContainerOrchestratorSocketFlagValue == nil || testContainerOrchestratorSocketFlagValue.SocketFilePath == "" {
		return nil
	}

	return containers.NewTestContainerOrchestratorClient(lifetimeCtx, log, testContainerOrchestratorSocketFlagValue.SocketFilePath)
}
