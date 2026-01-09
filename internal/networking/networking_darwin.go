/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build darwin

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
	// Default ephemeral port range on MacOS is 49152-65535.
	DefaultEphemeralPortRangeStart = 49152
	DefaultEphemeralPortRangeEnd   = 65535
)

func getEphemeralPortRange() (portRange, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), NetworkOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sysctl", "-n", "net.inet.ip.portrange.first", "net.inet.ip.portrange.last")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return portRange{DefaultEphemeralPortRangeStart, DefaultEphemeralPortRangeEnd}, false
	}

	return parseSysctlPortRange(stdout.Bytes())
}

func parseSysctlPortRange(buf []byte) (portRange, bool) {
	fields := strings.Fields(string(buf))
	if len(fields) != 2 {
		return portRange{DefaultEphemeralPortRangeStart, DefaultEphemeralPortRangeEnd}, false
	}

	startPort, err1 := strconv.Atoi(fields[0])
	endPort, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return portRange{DefaultEphemeralPortRangeStart, DefaultEphemeralPortRangeEnd}, false
	}

	return portRange{startPort, endPort}, true
}
