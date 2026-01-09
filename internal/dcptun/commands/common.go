/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/internal/dcptun"
)

const (
	gracefulShutdownTimeout = 5 * time.Second

	securityFlagsUsage = "[--ca-cert cert] [--server-cert cert] [--server-key key]"
)

var (
	tunnelConfig = dcptun.TunnelProxyConfig{}
)

func addSecurityFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&tunnelConfig.CACertBase64, "ca-cert", "", "Base64-encoded CA certificate for securing the control connection. If not provided, an insecure connection will be used (NOT RECOMMENDED!).")
	cmd.Flags().StringVar(&tunnelConfig.ServerCertBase64, "server-cert", "", "Base64-encoded server certificate for securing the control connection. Required if --ca-cert is provided.")
	cmd.Flags().StringVar(&tunnelConfig.ServerKeyBase64, "server-key", "", "Base64-encoded server private key for securing the control connection. Required if --ca-cert is provided.")
}

func validateSecurityFlagValues() error {
	// Must provide either all or none of the certificate parameters
	if tunnelConfig.HasCompleteCertificateData() {
		return nil
	}

	if tunnelConfig.CACertBase64 == "" && tunnelConfig.ServerCertBase64 == "" && tunnelConfig.ServerKeyBase64 == "" {
		return nil
	}

	return fmt.Errorf("all certificate parameters (--ca-cert, --server-cert, --server-key) must be provided together, or none at all")
}
