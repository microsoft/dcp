package azdRenderer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
)

const (
	noRootDir = "Root directory of the application not specified (--root-dir parameter is required)"
)

var (
	canRenderFlags rendererData
)

func NewCanRenderCommand() *cobra.Command {
	canRenderCmd := &cobra.Command{
		Use:   "can-render",
		Short: "Determines whether a given application can be rendered by this extension",
		RunE:  runCanRender,
		Args:  cobra.NoArgs,
	}

	addRootDirFlag(canRenderCmd, &canRenderFlags.appRootDir)

	return canRenderCmd
}

func runCanRender(cmd *cobra.Command, _ []string) error {
	result, reason, err := canRender(canRenderFlags.appRootDir)
	if err != nil {
		return err
	}

	err = writeResponse(result, reason, cmd.OutOrStdout())
	if err != nil {
		return err
	}

	return nil
}

func canRender(appRootDir string) (extensions.CanRenderResult, string, error) {
	if appRootDir == "" {
		return extensions.CanRenderResultNo, noRootDir, fmt.Errorf(noRootDir)
	}

	// If "azure.yaml" file is in the root, we are good to go
	azureYamlPath := filepath.Join(appRootDir, "azure.yaml")
	if _, err := os.Stat(azureYamlPath); err != nil {
		responseErr := fmt.Errorf("Folder '%s' does not seem to be a root of Azure Dev CLI-enabled application: %w", canRenderFlags.appRootDir, err)
		return extensions.CanRenderResultNo, responseErr.Error(), nil // The answer is no, but there was no unexpected error while determining that.
	}

	return extensions.CanRenderResultYes, "", nil
}

func writeResponse(result extensions.CanRenderResult, reason string, writer io.Writer) error {
	res := extensions.CanRenderResponse{}
	res.Result = result
	res.Reason = reason

	output, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("could not create the response document: %w", err)
	}

	_, err = writer.Write(commands.WithNewline(output))
	if err != nil {
		return fmt.Errorf("could not write the response document: %w", err)
	}

	return nil
}
