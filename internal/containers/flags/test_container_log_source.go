package flags

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

const TestContainerLogSourceFlagName = "test-container-log-source"

type TestContainerLogSourceFlagValue struct {
	SocketFilePath string
}

var testContainerLogSourceFlagValue *TestContainerLogSourceFlagValue = nil

func EnsureTestContainerLogSourceFlag(flags *pflag.FlagSet) {
	flags.Var(testContainerLogSourceFlagValue, TestContainerLogSourceFlagName, "Used to specify the path of the Unix domain socket file for the test container orchestrator, which serves as the container log source.")
	_ = flags.MarkHidden(TestContainerLogSourceFlagName)
}

func (lsfv *TestContainerLogSourceFlagValue) Set(val string) error {
	// If the value does not point to a valid file, that is an error
	file, err := os.Stat(val)
	if err != nil {
		return fmt.Errorf("invalid log source file path: %s", err)
	}
	if file.IsDir() {
		return fmt.Errorf("log source path '%s' is a directory", val)
	}

	testContainerLogSourceFlagValue = &TestContainerLogSourceFlagValue{
		SocketFilePath: val,
	}
	return nil
}

func (lsfv *TestContainerLogSourceFlagValue) String() string {
	if lsfv == nil {
		return ""
	} else {
		return lsfv.SocketFilePath
	}
}

func (lsfv *TestContainerLogSourceFlagValue) Type() string {
	return TestContainerLogSourceFlagName
}

func TryGetTestContainerLogSource(lifetimeCtx context.Context, log logr.Logger) containers.ContainerLogSource {
	if testContainerLogSourceFlagValue == nil || testContainerLogSourceFlagValue.SocketFilePath == "" {
		return nil
	}

	return containers.NewTestContainerOrchestratorClient(lifetimeCtx, log, testContainerLogSourceFlagValue.SocketFilePath)
}
