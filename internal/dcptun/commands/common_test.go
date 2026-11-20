/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/dcptun"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// Verifies that loadControlSecurityConfig fails closed when no control security
// configuration source is provided.
func TestLoadControlSecurityConfigFailsClosed(t *testing.T) {
	resetControlSecurityFlags(t)
	command := &cobra.Command{}

	config, configErr := loadControlSecurityConfig(command, false)
	require.Nil(t, config)
	require.ErrorContains(t, configErr, "control authentication is required")
}

// Verifies that stdin security bootstrap is decoded and validates outgoing
// credentials only for roles that initiate control RPCs.
func TestLoadControlSecurityConfigFromStdin(t *testing.T) {
	resetControlSecurityFlags(t)
	pairConfig, pairConfigErr := dcptun.NewTunnelProxySecurityConfig()
	require.NoError(t, pairConfigErr)
	clientConfigBytes, marshalErr := json.Marshal(pairConfig.ClientProxyConfig())
	require.NoError(t, marshalErr)
	command := &cobra.Command{}
	command.SetIn(bytes.NewReader(clientConfigBytes))
	controlSecurityStdin = true

	clientConfig, clientConfigErr := loadControlSecurityConfig(command, false)
	require.NoError(t, clientConfigErr)
	require.NotNil(t, clientConfig)

	command.SetIn(bytes.NewReader(clientConfigBytes))
	serverConfig, serverConfigErr := loadControlSecurityConfig(command, true)
	require.Nil(t, serverConfig)
	require.ErrorContains(t, serverConfigErr, "peer CA certificate data is required")

	controllerConfigBytes, controllerMarshalErr := json.Marshal(pairConfig.ControllerConfig())
	require.NoError(t, controllerMarshalErr)
	command.SetIn(bytes.NewReader(controllerConfigBytes))
	controllerAsServer, controllerAsServerErr := loadControlSecurityConfig(command, false)
	require.Nil(t, controllerAsServer)
	require.ErrorContains(t, controllerAsServerErr, "insufficient server certificate and key data")

	serverConfigBytes, serverMarshalErr := json.Marshal(pairConfig.ServerProxyConfig())
	require.NoError(t, serverMarshalErr)
	command.SetIn(bytes.NewReader(serverConfigBytes))
	loadedServerConfig, loadedServerConfigErr := loadControlSecurityConfig(command, true)
	require.NoError(t, loadedServerConfigErr)
	require.NotNil(t, loadedServerConfig)
}

// Verifies that loadControlSecurityConfig reads the client-proxy bootstrap from --control-security-file
// and rejects conflicting flags, a missing file, malformed content, and content exceeding the size limit.
func TestLoadControlSecurityConfigFromFile(t *testing.T) {
	resetControlSecurityFlags(t)
	pairConfig, pairConfigErr := dcptun.NewTunnelProxySecurityConfig()
	require.NoError(t, pairConfigErr)
	clientConfigBytes, marshalErr := json.Marshal(pairConfig.ClientProxyConfig())
	require.NoError(t, marshalErr)
	securityDirectory := t.TempDir()
	writeSecurityFile := func(name string, contents []byte) string {
		path := filepath.Join(securityDirectory, name)
		require.NoError(t, usvc_io.WriteFile(path, contents, osutil.PermissionOnlyOwnerReadWrite))
		return path
	}
	command := &cobra.Command{}

	controlSecurityFile = writeSecurityFile("client.json", clientConfigBytes)
	clientConfig, clientConfigErr := loadControlSecurityConfig(command, false)
	require.NoError(t, clientConfigErr)
	require.Equal(t, pairConfig.ClientProxyConfig(), *clientConfig)

	controlSecurityStdin = true
	conflictingConfig, conflictErr := loadControlSecurityConfig(command, false)
	require.Nil(t, conflictingConfig)
	require.ErrorContains(t, conflictErr, "cannot be used together")
	controlSecurityStdin = false

	controlSecurityFile = filepath.Join(securityDirectory, "missing.json")
	missingConfig, missingErr := loadControlSecurityConfig(command, false)
	require.Nil(t, missingConfig)
	require.ErrorIs(t, missingErr, os.ErrNotExist)

	controlSecurityFile = writeSecurityFile("malformed.json", []byte("{not json"))
	malformedConfig, malformedErr := loadControlSecurityConfig(command, false)
	require.Nil(t, malformedConfig)
	require.ErrorContains(t, malformedErr, "decode control security configuration")

	oversized := []byte(`{"serverCertBase64":"` + strings.Repeat("A", 1024*1024) + `"}`)
	controlSecurityFile = writeSecurityFile("oversized.json", oversized)
	oversizedConfig, oversizedErr := loadControlSecurityConfig(command, false)
	require.Nil(t, oversizedConfig)
	require.ErrorContains(t, oversizedErr, "decode control security configuration")
}

func resetControlSecurityFlags(t *testing.T) {
	t.Helper()
	originalFile := controlSecurityFile
	originalStdin := controlSecurityStdin
	controlSecurityFile = ""
	controlSecurityStdin = false
	t.Cleanup(func() {
		controlSecurityFile = originalFile
		controlSecurityStdin = originalStdin
	})
}
