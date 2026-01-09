/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Returns true if the environment variable "switch" is enabled.
// The environment variable is considered enabled if it is set to one of the "truthy" values:
// "1", "true", "on", or "yes".
func EnvVarSwitchEnabled(varName string) bool {
	value, found := os.LookupEnv(varName)
	if !found || strings.TrimSpace(value) == "" {
		return false
	}

	value = strings.TrimSpace(value)
	enabled := strings.EqualFold(value, "1") ||
		strings.EqualFold(value, "true") ||
		strings.EqualFold(value, "on") ||
		strings.EqualFold(value, "yes")
	return enabled
}

func EnvVarIntVal(varName string) (int, bool) {
	value, found := os.LookupEnv(varName)
	if !found || strings.TrimSpace(value) == "" {
		return 0, false
	}

	value = strings.TrimSpace(value)
	val, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, false
	}

	return int(val), true
}

func EnvVarStringWithDefault(varName string, defaultVal string) string {
	val, found := os.LookupEnv(varName)
	if !found || strings.TrimSpace(val) == "" {
		return defaultVal
	} else {
		return val
	}
}

func EnvVarIntValWithDefault(varName string, defaultVal int) int {
	val, found := EnvVarIntVal(varName)
	if !found {
		return defaultVal
	} else {
		return val
	}
}

func EnvVarDurationValWithDefault(varName string, defaultVal time.Duration) time.Duration {
	value, found := os.LookupEnv(varName)
	if !found || strings.TrimSpace(value) == "" {
		return defaultVal
	}

	value = strings.TrimSpace(value)
	val, err := time.ParseDuration(value)
	if err != nil {
		return defaultVal
	}

	return val
}
