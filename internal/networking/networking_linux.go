//go:build !windows && !darwin

package networking

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// Default ephemeral port range on Linux is 32768-60999.
	DefaultEphemeralPortRangeStart = 32768
	DefaultEphemeralPortRangeEnd   = 60999
)

func getEphemeralPortRange() (portRange, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), NetworkOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sysctl", "-n", "net.ipv4.ip_local_port_range")

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
