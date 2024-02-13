package commands

import (
	"os"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func ErrorExit(log logger.Logger, err error, exitCode int) {
	log.Error(err, "the program finished with an error", "ExitCode", exitCode)
	log.Flush()
	os.Exit(exitCode)
}
