/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFilteredAmbientEnvExcludesSuppressedPrefixes(t *testing.T) {
	// Set environment variables that should be suppressed.
	t.Setenv("DCP_TEST_VAR", "should_be_removed")
	t.Setenv("DCP_ANOTHER", "also_removed")
	t.Setenv("DEBUG_SESSION_ID", "removed_too")
	// Set a normal variable that should survive.
	t.Setenv("MY_APP_SETTING", "keep_me")

	envMap := NewFilteredAmbientEnv()

	_, hasDcpTest := envMap.Get("DCP_TEST_VAR")
	assert.False(t, hasDcpTest, "DCP_TEST_VAR should be suppressed")

	_, hasDcpAnother := envMap.Get("DCP_ANOTHER")
	assert.False(t, hasDcpAnother, "DCP_ANOTHER should be suppressed")

	_, hasDebugSession := envMap.Get("DEBUG_SESSION_ID")
	assert.False(t, hasDebugSession, "DEBUG_SESSION_ID should be suppressed")

	val, hasAppSetting := envMap.Get("MY_APP_SETTING")
	assert.True(t, hasAppSetting, "MY_APP_SETTING should be present")
	assert.Equal(t, "keep_me", val)
}

func TestNewFilteredAmbientEnvContainsNormalVars(t *testing.T) {
	// PATH should always exist and not be suppressed.
	pathVal, found := os.LookupEnv("PATH")
	if !found {
		t.Skip("PATH not set in test environment")
	}

	envMap := NewFilteredAmbientEnv()

	got, ok := envMap.Get("PATH")
	require.True(t, ok, "PATH should be present in the filtered env")
	assert.Equal(t, pathVal, got)
}

func TestNewFilteredAmbientEnvHasNoSuppressedKeys(t *testing.T) {
	t.Setenv("DCP_SOME_KEY", "value")
	t.Setenv("DEBUG_SESSION_TOKEN", "value")

	envMap := NewFilteredAmbientEnv()

	for key := range envMap.Data() {
		for _, prefix := range SuppressedEnvVarPrefixes {
			assert.Falsef(t, strings.HasPrefix(key, prefix),
				"key %q should have been suppressed (prefix %q)", key, prefix)
		}
	}
}

func TestSuppressEnvVarPrefixesRemovesMatchingKeys(t *testing.T) {
	envMap := NewPlatformStringMap[string]()
	envMap.Set("DCP_FOO", "1")
	envMap.Set("DEBUG_SESSION_BAR", "2")
	envMap.Set("KEEP_ME", "3")

	SuppressEnvVarPrefixes(envMap)

	_, hasDcp := envMap.Get("DCP_FOO")
	assert.False(t, hasDcp)

	_, hasDebug := envMap.Get("DEBUG_SESSION_BAR")
	assert.False(t, hasDebug)

	val, hasKeep := envMap.Get("KEEP_ME")
	assert.True(t, hasKeep)
	assert.Equal(t, "3", val)
}

func TestNewPlatformStringMapMode(t *testing.T) {
	m := NewPlatformStringMap[string]()
	m.Set("TestKey", "value")

	if IsWindows() {
		// Case-insensitive: looking up with different casing should succeed.
		val, ok := m.Get("testkey")
		assert.True(t, ok, "expected case-insensitive lookup on Windows")
		assert.Equal(t, "value", val)
	} else {
		// Case-sensitive: different casing should NOT match.
		_, ok := m.Get("testkey")
		assert.False(t, ok, "expected case-sensitive lookup on non-Windows")
	}
}
