/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"io"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/stretchr/testify/require"
)

type protocolTestCase struct {
	name     string
	protocol commonapi.PortProtocol
}

var (
	protocolTestCases = []protocolTestCase{
		{
			name:     "TCP",
			protocol: commonapi.PortProtocolTCP,
		},
		{
			name:     "UDP",
			protocol: commonapi.PortProtocolUDP,
		},
	}

	log = logger.New("networking-tests").Logger
)

func TestMain(m *testing.M) {
	EnableStrictMruPortHandling(log)
	code := m.Run()
	os.Exit(code)
}

func TestGetFreePort(t *testing.T) {
	t.Parallel()

	for _, tc := range protocolTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			port1, err1 := GetFreePort(tc.protocol, Localhost, log)
			port2, err2 := GetFreePort(tc.protocol, Localhost, log)

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

			ips, err := LookupIP(Localhost)
			require.NoError(t, err, "Could not get IP addresses for localhost")

			for _, ip := range ips {
				address := IpToString(ip)
				_, err = GetFreePort(tc.protocol, address, log)
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

			ips, ipLookupErr := LookupIP(Localhost)
			require.NoError(t, ipLookupErr, "Could not get IP addresses for localhost")

			wg := sync.WaitGroup{}
			wg.Add(len(ips))

			for _, ip := range ips {
				go func(ip net.IP) {
					defer wg.Done()
					address := IpToString(ip)
					port, err := GetFreePort(tc.protocol, address, log)
					require.NoError(t, err, "Could not get free port for address %s", address)

					err = CheckPortAvailable(tc.protocol, address, port, log)
					require.NoError(t, err, "Port %d on address %s is not available", port, address)

					// Occupy the port
					var listener io.Closer
					ap := AddressAndPort(address, port)

					if tc.protocol == commonapi.PortProtocolUDP {
						udpaddr, resolutionErr := net.ResolveUDPAddr("udp", ap)
						require.NoError(t, resolutionErr, "Could not resolve UDP address %s", ap)
						listener, err = net.ListenUDP("udp", udpaddr)
						require.NoError(t, err, "Could not listen on UDP address %s", ap)
					} else {
						tcpaddr, resolutionErr := net.ResolveTCPAddr("tcp", ap)
						require.NoError(t, resolutionErr, "Could not resolve TCP address %s", ap)
						listener, err = net.ListenTCP("tcp", tcpaddr)
						require.NoError(t, err, "Could not listen on TCP address %s", ap)
					}

					err = CheckPortAvailable(tc.protocol, address, port, log)
					require.Error(t, err, "Port %d on address %s is available", port, address)

					err = listener.Close()
					require.NoError(t, err, "Could not close listener on port %d on address %s", port, address)

					err = CheckPortAvailable(tc.protocol, address, port, log)
					require.NoError(t, err, "Port %d on address %s is not available", port, address)
				}(ip)
			}

			wg.Wait()
		})
	}
}

// Verifies that IpToString brackets native IPv6 addresses without depending on host network
// capabilities and preserves IPv4 formatting, including IPv4-mapped addresses.
func TestIpToString(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		address  string
		expected string
	}{
		{IPv4LocalhostDefaultAddress, IPv4LocalhostDefaultAddress},
		{net.IPv6loopback.String(), IPv6LocalhostDefaultAddress},
		{net.IPv6zero.String(), IPv6AllInterfaceAddress},
		{"2001:db8::1", "[2001:db8::1]"},
		{"::ffff:127.0.0.1", IPv4LocalhostDefaultAddress},
	} {
		t.Run(testCase.address, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, testCase.expected, IpToString(net.ParseIP(testCase.address)))
		})
	}
}

// Verifies that AddressAndPort preserves IPv4/hostname authorities and the bracketed
// representation of IPv6 literals, including mapped and zoned addresses.
func TestAddressAndPortIPv6Formatting(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		address  string
		expected string
		host     string
	}{
		{IPv4LocalhostDefaultAddress, IPv4LocalhostDefaultAddress + ":5001", IPv4LocalhostDefaultAddress},
		{Localhost, Localhost + ":5001", Localhost},
		{"service.example", "service.example:5001", "service.example"},
		{IPv6LocalhostDefaultAddress, IPv6LocalhostDefaultAddress + ":5001", net.IPv6loopback.String()},
		{"[2001:db8::1]", "[2001:db8::1]:5001", "2001:db8::1"},
		{"[::ffff:127.0.0.1]", "[::ffff:127.0.0.1]:5001", "::ffff:127.0.0.1"},
		{"[fe80::1%lo0]", "[fe80::1%lo0]:5001", "fe80::1%lo0"},
	} {
		t.Run(testCase.address, func(t *testing.T) {
			t.Parallel()
			authority := AddressAndPort(testCase.address, 5001)
			require.Equal(t, testCase.expected, authority)
			host, port, splitErr := net.SplitHostPort(authority)
			require.NoError(t, splitErr)
			require.Equal(t, testCase.host, host)
			require.Equal(t, "5001", port)
		})
	}
}

// Verifies that TCP and UDP address parsers accept authorities built from bracketed IPv6 endpoints,
// matching the formatting boundary used by the runtime proxy without opening network connections.
func TestAddressAndPortParsesIPv6ProxyEndpoint(t *testing.T) {
	t.Parallel()
	for _, address := range []string{IPv6LocalhostDefaultAddress, "[2001:db8::1]"} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()
			authority := AddressAndPort(address, 5001)
			tcpAddress, tcpErr := net.ResolveTCPAddr("tcp", authority)
			require.NoError(t, tcpErr)
			require.True(t, net.ParseIP(ToStandaloneAddress(address)).Equal(tcpAddress.IP))
			require.Equal(t, 5001, tcpAddress.Port)
			udpAddress, udpErr := net.ResolveUDPAddr("udp", authority)
			require.NoError(t, udpErr)
			require.True(t, net.ParseIP(ToStandaloneAddress(address)).Equal(udpAddress.IP))
			require.Equal(t, 5001, udpAddress.Port)
		})
	}
}
