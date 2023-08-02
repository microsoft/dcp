package azdRenderer

import (
	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/pkg/extensions"
)

const azdRendererID = "azure-dev-cli"

func NewGetCapabilitiesCommand() *cobra.Command {
	getCapabilitiesCmd := &cobra.Command{
		Use:   "get-capabilities",
		Short: "Returns the role for this DCP extension (Azure Developer CLI application runner)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return extensions.WriteCapabiltiesDoc(
				cmd.OutOrStdout(),
				"Azure Developer CLI application runner",
				azdRendererID,
				[]extensions.ExtensionCapability{extensions.WorkloadRendererCapability},
			)
		},
		Args: cobra.NoArgs,
	}

	return getCapabilitiesCmd
}
