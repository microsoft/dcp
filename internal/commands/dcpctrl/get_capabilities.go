package dcpctrl

import (
	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/pkg/extensions"
)

func NewGetCapabilitiesCommand() *cobra.Command {
	getCapabilitiesCmd := &cobra.Command{
		Use:   "get-capabilities",
		Short: "Returns the role for this DCP extension (controller host)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return extensions.WriteCapabiltiesDoc(cmd.OutOrStdout(), "DCP controller host", "dcpctrl", []extensions.ExtensionCapability{
				extensions.ControllerCapability,
			})
		},
		Args: cobra.NoArgs,
	}

	return getCapabilitiesCmd
}
