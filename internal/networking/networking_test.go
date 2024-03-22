// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/stretchr/testify/require"
)

type protocolTestCase struct {
	name     string
	protocol apiv1.PortProtocol
}

var protocolTestCases = []protocolTestCase{
	{
		name:     "TCP",
		protocol: apiv1.TCP,
	},
	{
		name:     "UDP",
		protocol: apiv1.UDP,
	},
}

func TestGetFreePort(t *testing.T) {
	t.Parallel()

	for _, tc := range protocolTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			port1, err1 := GetFreePort(tc.protocol, "localhost")
			port2, err2 := GetFreePort(tc.protocol, "localhost")

			require.NoError(t, err1, "error1: %v", err1)
			require.NoError(t, err2, "error2: %v", err2)

			require.NotEqual(t, port1, port2, "GetFreePort must not return the same port when called twice in immediate succession")
		})
	}
}

func TestCanGetFreePortForAllLocalIPs(t *testing.T) {
	t.Parallel()

	for _, tc := range protocolTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ips, err := LookupIP("localhost")
			require.NoError(t, err, "Could not get IP addresses for localhost")

			for _, ip := range ips {
				address := IpToString(ip)
				_, err = GetFreePort(tc.protocol, address)
				require.NoError(t, err, "Could not get free port for address %s", address)
			}
		})
	}

}

func TestCheckPortAvailable(t *testing.T) {
	t.Parallel()

	for _, tc := range protocolTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ips, ipLookupErr := LookupIP("localhost")
			require.NoError(t, ipLookupErr, "Could not get IP addresses for localhost")

			wg := sync.WaitGroup{}
			wg.Add(len(ips))

			for _, ip := range ips {
				go func(ip net.IP) {
					defer wg.Done()
					address := IpToString(ip)
					port, err := GetFreePort(tc.protocol, address)
					require.NoError(t, err, "Could not get free port for address %s", address)

					err = CheckPortAvailable(tc.protocol, address, port)
					require.NoError(t, err, "Port %d on address %s is not available", port, address)

					// Occupy the port
					var listener io.Closer
					if tc.protocol == apiv1.UDP {
						udpaddr, resolutionErr := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", address, port))
						require.NoError(t, resolutionErr, "Could not resolve UDP address %s:%d", address, port)
						listener, err = net.ListenUDP("udp", udpaddr)
						require.NoError(t, err, "Could not listen on UDP address %s:%d", address, port)
					} else {
						tcpaddr, resolutionErr := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", address, port))
						require.NoError(t, resolutionErr, "Could not resolve TCP address %s:%d", address, port)
						listener, err = net.ListenTCP("tcp", tcpaddr)
						require.NoError(t, err, "Could not listen on TCP address %s:%d", address, port)
					}

					err = CheckPortAvailable(tc.protocol, address, port)
					require.Error(t, err, "Port %d on address %s is available", port, address)

					err = listener.Close()
					require.NoError(t, err, "Could not close listener on port %d on address %s", port, address)

					err = CheckPortAvailable(tc.protocol, address, port)
					require.NoError(t, err, "Port %d on address %s is not available", port, address)
				}(ip)
			}

			wg.Wait()
		})
	}
}
