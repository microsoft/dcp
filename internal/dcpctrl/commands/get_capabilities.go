/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/pkg/extensions"
)

func NewGetCapabilitiesCommand(log logr.Logger) *cobra.Command {
	getCapabilitiesCmd := &cobra.Command{
		Use:   "get-capabilities",
		Short: "Returns the role for this DCP extension (controller host)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return extensions.WriteCapabiltiesDoc(cmd.OutOrStdout(), "DCP controller host", "dcpctrl", []extensions.ExtensionCapability{
				extensions.ControllerCapability,
				extensions.ProcessMonitorCapability,
			})
		},
		Args: cobra.NoArgs,
	}

	return getCapabilitiesCmd
}
