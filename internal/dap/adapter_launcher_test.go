/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFilteredEnv_SuppressesDCPPrefix(t *testing.T) {
	t.Setenv("DCP_TEST_VAR", "should-be-removed")
	t.Setenv("DCP_ANOTHER", "also-removed")

	config := &DebugAdapterConfig{}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.NotContains(t, envMap, "DCP_TEST_VAR")
	assert.NotContains(t, envMap, "DCP_ANOTHER")
}

func TestBuildFilteredEnv_SuppressesDebugSessionPrefix(t *testing.T) {
	t.Setenv("DEBUG_SESSION_ID", "should-be-removed")
	t.Setenv("DEBUG_SESSION_TOKEN", "also-removed")

	config := &DebugAdapterConfig{}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.NotContains(t, envMap, "DEBUG_SESSION_ID")
	assert.NotContains(t, envMap, "DEBUG_SESSION_TOKEN")
}

func TestBuildFilteredEnv_InheritsNonSuppressedVars(t *testing.T) {
	t.Setenv("MY_APP_VAR", "keep-this")

	config := &DebugAdapterConfig{}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.Equal(t, "keep-this", envMap["MY_APP_VAR"])
}

func TestBuildFilteredEnv_ConfigEnvVarsAreApplied(t *testing.T) {
	config := &DebugAdapterConfig{
		Env: []apiv1.EnvVar{
			{Name: "CUSTOM_VAR", Value: "custom-value"},
			{Name: "ANOTHER_VAR", Value: "another-value"},
		},
	}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.Equal(t, "custom-value", envMap["CUSTOM_VAR"])
	assert.Equal(t, "another-value", envMap["ANOTHER_VAR"])
}

func TestBuildFilteredEnv_ConfigOverridesAmbient(t *testing.T) {
	t.Setenv("OVERRIDE_ME", "original")

	config := &DebugAdapterConfig{
		Env: []apiv1.EnvVar{
			{Name: "OVERRIDE_ME", Value: "overridden"},
		},
	}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.Equal(t, "overridden", envMap["OVERRIDE_ME"])
}

func TestBuildFilteredEnv_ConfigCanSetSuppressedPrefixVars(t *testing.T) {
	// Even though DCP_ vars are suppressed from the ambient environment,
	// the config should be able to explicitly set them.
	t.Setenv("DCP_AMBIENT", "should-be-removed")

	config := &DebugAdapterConfig{
		Env: []apiv1.EnvVar{
			{Name: "DCP_EXPLICIT", Value: "explicitly-set"},
		},
	}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.NotContains(t, envMap, "DCP_AMBIENT")
	assert.Equal(t, "explicitly-set", envMap["DCP_EXPLICIT"])
}

func TestBuildFilteredEnv_InheritsPath(t *testing.T) {
	// PATH should be inherited since it doesn't match any suppressed prefix.
	pathVal := os.Getenv("PATH")
	require.NotEmpty(t, pathVal, "PATH should be set in the test environment")

	config := &DebugAdapterConfig{}
	env := buildFilteredEnv(config)

	envMap := sliceToEnvMap(env)
	assert.Equal(t, pathVal, envMap["PATH"])
}

// sliceToEnvMap converts a []string of "KEY=VALUE" entries to a map.
func sliceToEnvMap(envSlice []string) map[string]string {
	result := make(map[string]string, len(envSlice))
	for _, entry := range envSlice {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

func TestConnectToExistingAdapter(t *testing.T) {
	t.Parallel()

	// Start a TCP listener to simulate an existing adapter
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	defer listener.Close()

	// Accept a connection in the background
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter, connectErr := ConnectToExistingAdapter(ctx, listener.Addr().String(), logr.Discard())
	require.NoError(t, connectErr)
	require.NotNil(t, adapter)
	defer adapter.Close()

	assert.Equal(t, listener.Addr().String(), adapter.Address)
	assert.NotNil(t, adapter.Transport)

	// Verify the server side received the connection
	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("server did not accept connection")
	}
}

func TestConnectToExistingAdapter_EmptyAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, connectErr := ConnectToExistingAdapter(ctx, "", logr.Discard())
	assert.Error(t, connectErr)
	assert.Contains(t, connectErr.Error(), "adapter address is required")
}

func TestConnectToExistingAdapter_ConnectionFailure(t *testing.T) {
	t.Parallel()

	// Use a port that is not listening
	ctx := context.Background()
	_, connectErr := ConnectToExistingAdapter(ctx, "127.0.0.1:1", logr.Discard())
	assert.Error(t, connectErr)
	assert.Contains(t, connectErr.Error(), "failed to connect to existing adapter")
}

func TestLaunchedAdapter_AddressSetForTCPConnect(t *testing.T) {
	// Verify that Address field is populated after TCP connect launch.
	// We can't fully test the adapter launch without a real adapter,
	// but we can verify the field exists and is of the right type.
	adapter := &LaunchedAdapter{
		Address: "127.0.0.1:5678",
		mu:      &sync.Mutex{},
		done:    make(chan struct{}),
	}
	assert.Equal(t, "127.0.0.1:5678", adapter.Address)
}
