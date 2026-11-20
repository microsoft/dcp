/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/internal/dcptun"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

const (
	gracefulShutdownTimeout = 5 * time.Second

	securityFlagsUsage = "(--control-security-file path | --control-security-stdin)"
)

var (
	tunnelConfig         = dcptun.TunnelProxyConfig{}
	controlSecurityFile  string
	controlSecurityStdin bool
)

func addSecurityFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&controlSecurityFile, "control-security-file", "", "Path to the control security configuration file.")
	cmd.Flags().BoolVar(&controlSecurityStdin, "control-security-stdin", false, "Read the control security configuration from standard input.")
}

// loadControlSecurityConfig loads TunnelControlSecurityConfig from either --control-security-file or stdin
// and validates the server-proxy or client-proxy credentials required to run the proxy.
// Missing, conflicting, or invalid configuration must not permit unauthenticated startup.
func loadControlSecurityConfig(cmd *cobra.Command, requireClientCredentials bool) (*dcptun.TunnelControlSecurityConfig, error) {
	if controlSecurityFile != "" && controlSecurityStdin {
		return nil, fmt.Errorf("--control-security-file and --control-security-stdin cannot be used together")
	}
	if controlSecurityFile == "" && !controlSecurityStdin {
		return nil, fmt.Errorf("control authentication is required; provide --control-security-file or --control-security-stdin")
	}

	var reader io.Reader
	var closeReader func() error
	if controlSecurityStdin {
		reader = cmd.InOrStdin()
	} else {
		securityFile, openErr := usvc_io.OpenFileReadOnly(controlSecurityFile)
		if openErr != nil {
			return nil, fmt.Errorf("open control security configuration: %w", openErr)
		}
		reader = securityFile
		closeReader = securityFile.Close
	}
	if closeReader != nil {
		defer func() { _ = closeReader() }()
	}

	configBytes, readErr := io.ReadAll(io.LimitReader(reader, 1024*1024))
	if readErr != nil {
		return nil, fmt.Errorf("read control security configuration: %w", readErr)
	}
	var securityConfig dcptun.TunnelControlSecurityConfig
	if unmarshalErr := json.Unmarshal(configBytes, &securityConfig); unmarshalErr != nil {
		return nil, fmt.Errorf("decode control security configuration: %w", unmarshalErr)
	}
	if validateServerErr := securityConfig.ValidateServer(); validateServerErr != nil {
		return nil, fmt.Errorf("validate control server security configuration: %w", validateServerErr)
	}
	if requireClientCredentials {
		if validateClientErr := securityConfig.ValidateClient(); validateClientErr != nil {
			return nil, fmt.Errorf("validate control client security configuration: %w", validateClientErr)
		}
	}
	return &securityConfig, nil
}
