/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"os"
	"strings"

	"github.com/microsoft/dcp/pkg/maps"
)

// SuppressedEnvVarPrefixes is the set of environment variable prefixes that should not
// be inherited from the ambient (DCP process) environment when launching child processes
// such as Executables and debug adapters. Variables whose names start with any of these
// prefixes are removed from the inherited environment.
var SuppressedEnvVarPrefixes = []string{
	"DEBUG_SESSION",
	"DCP_",
}

// NewFilteredAmbientEnv returns a StringKeyMap populated from the current process
// environment with all variables whose names match SuppressedEnvVarPrefixes removed.
// The returned map uses case-insensitive keys on Windows and case-sensitive keys
// on other platforms.
//
// Callers can overlay additional environment variables on top of the returned map
// (e.g. from configuration or spec) before converting it to the final []string
// used by exec.Cmd.Env.
func NewFilteredAmbientEnv() maps.StringKeyMap[string] {
	envMap := NewPlatformStringMap[string]()

	envMap.Apply(maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
		parts := strings.SplitN(envStr, "=", 2)
		return parts[0], parts[1]
	}))

	SuppressEnvVarPrefixes(envMap)

	return envMap
}

// NewPlatformStringMap returns a new empty StringKeyMap with the key-comparison mode
// appropriate for the current platform (case-insensitive on Windows, case-sensitive
// elsewhere).
func NewPlatformStringMap[T any]() maps.StringKeyMap[T] {
	if IsWindows() {
		return maps.NewStringKeyMap[T](maps.StringMapModeCaseInsensitive)
	}
	return maps.NewStringKeyMap[T](maps.StringMapModeCaseSensitive)
}

// SuppressEnvVarPrefixes removes all entries from envMap whose keys start with any
// of the SuppressedEnvVarPrefixes. This can be called at any point in an environment-
// building pipeline to strip DCP-internal variables.
func SuppressEnvVarPrefixes(envMap maps.StringKeyMap[string]) {
	for _, prefix := range SuppressedEnvVarPrefixes {
		envMap.DeletePrefix(prefix)
	}
}
