package commands

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/version"
)

const (
	defaultVersion = "dev"

	//  If set, the value of this variable will be written to the log file as one of the first log messages.
	DCP_LOGGING_CONTEXT = "DCP_LOGGING_CONTEXT"
)

var (
	Version        = defaultVersion
	CommitHash     = ""
	BuildTimestamp = ""
)

func NewVersionCommand(log logr.Logger) (*cobra.Command, error) {
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Prints version information",
		Long:  `Prints version information.`,
		RunE:  getVersion(log),
		Args:  cobra.NoArgs,
	}

	return versionCmd, nil
}

func getVersion(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("version")

		versionStr, err := versionString()
		if err != nil {
			log.Error(err, "Could not serialize version information")
			return err
		} else {
			fmt.Println(string(versionStr))
			return nil
		}
	}
}

func LogVersion(log logr.Logger, programStartMsg string) func(_ *cobra.Command, _ []string) {
	return func(_ *cobra.Command, _ []string) {
		versionString, err := versionString()
		if err != nil {
			versionString = fmt.Sprintf("unknown: %v", err)
		}

		launchPath, pathErr := os.Executable()
		if pathErr != nil {
			launchPath = os.Args[0]
		}

		args := os.Args[1:]

		log.V(1).Info(programStartMsg,
			"PID", os.Getpid(),
			"Exe", launchPath,
			"Args", args,
			"Version", versionString,
		)

		logContext, found := os.LookupEnv(DCP_LOGGING_CONTEXT)
		if found && len(logContext) > 0 {
			log.V(1).Info(logContext)
		}
	}
}

func versionString() (string, error) {
	if version, err := json.Marshal(version.Version()); err != nil {
		return "", err
	} else {
		return string(version), nil
	}
}
