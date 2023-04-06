package commands

import (
	"github.com/spf13/cobra"

	"github.com/usvc-dev/apiserver/pkg/extensions"
)

const StandardApiServerID = "dcpd"

func NewGetCapabilitiesCommand() *cobra.Command {
	getCapabilitiesCmd := &cobra.Command{
		Use:   "get-capabilities",
		Short: "Returns the role for this program (DCP API server)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return extensions.WriteCapabiltiesDoc(
				cmd.OutOrStdout(),
				"DCP API server",
				StandardApiServerID,
				[]extensions.ExtensionCapability{extensions.ApiServerCapability},
			)
		},
		Args: cobra.NoArgs,
	}

	return getCapabilitiesCmd
}
