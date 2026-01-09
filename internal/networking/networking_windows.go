/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// Default ephemeral port range on Windows is 49152-65535.
	DefaultEphemeralPortRangeStart = 49152
	DefaultEphemeralPortRangeEnd   = 65535
)

func getEphemeralPortRange() (portRange, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), NetworkOpTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "netsh", "interface", "ip", "show", "dynamicportrange", "tcp")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return portRange{DefaultEphemeralPortRangeStart, DefaultEphemeralPortRangeEnd}, false
	}

	return parseEphemeralPortRange(stdout.Bytes())
}

// parseEphemeralPortRange parses the output of 'netsh interface ip show dynamicportrange tcp'
// and returns the start and end port.
func parseEphemeralPortRange(buf []byte) (portRange, bool) {
	lines := strings.Split(string(buf), "\n")
	var startPort, numPorts int
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Start Port") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				if n, err := strconv.Atoi(val); err == nil {
					startPort = n
				}
			}
		}
		if strings.HasPrefix(line, "Number of Ports") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				if n, err := strconv.Atoi(val); err == nil {
					numPorts = n
				}
			}
		}
	}
	if startPort > 0 && numPorts > 0 {
		return portRange{startPort, startPort + numPorts - 1}, true
	}

	return portRange{DefaultEphemeralPortRangeStart, DefaultEphemeralPortRangeEnd}, false
}
