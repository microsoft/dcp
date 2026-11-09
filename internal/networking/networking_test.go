/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
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

			port1, err1 := GetFreePort(context.Background(), tc.protocol, Localhost, log)
			port2, err2 := GetFreePort(context.Background(), tc.protocol, Localhost, log)

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
				_, err = GetFreePort(context.Background(), tc.protocol, address, log)
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
					port, err := GetFreePort(context.Background(), tc.protocol, address, log)
					require.NoError(t, err, "Could not get free port for address %s", address)

					err = CheckPortAvailable(context.Background(), tc.protocol, address, port, log)
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

					err = CheckPortAvailable(context.Background(), tc.protocol, address, port, log)
					require.Error(t, err, "Port %d on address %s is available", port, address)

					err = listener.Close()
					require.NoError(t, err, "Could not close listener on port %d on address %s", port, address)

					err = CheckPortAvailable(context.Background(), tc.protocol, address, port, log)
					require.NoError(t, err, "Port %d on address %s is not available", port, address)
				}(ip)
			}

			wg.Wait()
		})
	}
}

func TestConfiguredPortAllocationRangesSubtractsEphemeralOverlap(t *testing.T) {
	ephemeralStart, ephemeralEnd, _ := GetEphemeralPortRange()
	if ephemeralStart <= 1 {
		t.Skip("ephemeral range starts too low to create a partially overlapping test range")
	}

	t.Setenv(DCP_PORT_ALLOCATION_RANGE, fmt.Sprintf("%d-%d", ephemeralStart-1, ephemeralStart+1))
	portRangeOverlapReported = false

	ranges, err := configuredPortAllocationRanges(false, log)

	require.NoError(t, err)
	for _, candidateRange := range ranges.ranges {
		require.True(t, candidateRange.End < ephemeralStart || candidateRange.Start > ephemeralEnd)
	}
}

func TestIndexedPortRangesNormalizeOverlappingRanges(t *testing.T) {
	t.Parallel()

	ranges := newIndexedPortRanges([]portRange{
		{Start: 26020, End: 26022},
		{Start: 26010, End: 26015},
		{Start: 26014, End: 26018},
		{Start: 26030, End: 26030},
		{Start: 26019, End: 26025},
	})

	require.Equal(t, 17, ranges.Len())
	require.Equal(t, []portRange{
		{Start: 26010, End: 26025},
		{Start: 26030, End: 26030},
	}, ranges.ranges)

	candidates := []int32{}
	for index := 0; index < ranges.Len(); index++ {
		candidate, ok := ranges.PortAt(index)
		require.True(t, ok)
		candidates = append(candidates, candidate)
	}

	require.Equal(t, []int32{
		26010, 26011, 26012, 26013, 26014, 26015, 26016, 26017, 26018,
		26019, 26020, 26021, 26022, 26023, 26024, 26025, 26030,
	}, candidates)

	_, beforeRangeOk := ranges.PortAt(-1)
	_, afterRangeOk := ranges.PortAt(ranges.Len())
	require.False(t, beforeRangeOk)
	require.False(t, afterRangeOk)
}

func TestPortAllocationCandidatesCoverRange(t *testing.T) {
	t.Parallel()

	ranges := newIndexedPortRanges([]portRange{
		{Start: 26010, End: 26011},
		{Start: 26020, End: 26022},
	})

	candidateIterator := newStateStorePortAllocationCandidateIterator(ranges, string(commonapi.PortProtocolTCP), IPv4LocalhostDefaultAddress)
	candidates := []int32{}
	for {
		candidate, ok := candidateIterator.Next()
		if !ok {
			break
		}
		candidates = append(candidates, candidate)
	}

	require.Len(t, candidates, 5)
	require.ElementsMatch(t, []int32{26010, 26011, 26020, 26021, 26022}, candidates)
}

func TestPortAllocationCandidatesCoverOverlappingRangesOnce(t *testing.T) {
	t.Parallel()

	ranges := newIndexedPortRanges([]portRange{
		{Start: 26010, End: 26015},
		{Start: 26013, End: 26017},
		{Start: 26020, End: 26021},
	})

	candidateIterator := newStateStorePortAllocationCandidateIterator(ranges, string(commonapi.PortProtocolTCP), IPv4LocalhostDefaultAddress)
	candidates := []int32{}
	for {
		candidate, ok := candidateIterator.Next()
		if !ok {
			break
		}
		candidates = append(candidates, candidate)
	}

	require.Len(t, candidates, 10)
	require.ElementsMatch(t, []int32{26010, 26011, 26012, 26013, 26014, 26015, 26016, 26017, 26020, 26021}, candidates)
}

func TestPortAllocationCandidateStepCoversEveryIndex(t *testing.T) {
	t.Parallel()

	for total := 2; total <= 100; total++ {
		step := stateStorePortAllocationCandidateStep(total, uint64(total*17))
		seen := map[int]struct{}{}
		for next := 0; next < total; next++ {
			seen[(next*step)%total] = struct{}{}
		}

		require.Len(t, seen, total)
	}
}

func TestNormalizePortAllocationAddressUsesIPForLocalhost(t *testing.T) {
	t.Parallel()

	address, err := NormalizePortAllocationAddress(Localhost)

	require.NoError(t, err)
	require.NotEqual(t, Localhost, address)
	_, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	require.NoError(t, parseErr)
}

func TestNormalizePortAllocationAddressCanonicalizesIPs(t *testing.T) {
	t.Parallel()

	ipv4MappedAddress, ipv4MappedErr := NormalizePortAllocationAddress("[::ffff:127.0.0.1]")
	require.NoError(t, ipv4MappedErr)
	require.Equal(t, IPv4LocalhostDefaultAddress, ipv4MappedAddress)

	ipv6Address, ipv6Err := NormalizePortAllocationAddress("[0:0:0:0:0:0:0:1]")
	require.NoError(t, ipv6Err)
	require.Equal(t, IPv6LocalhostDefaultAddress, ipv6Address)
}

func TestGetFreePortWithStateStoreUsesConfiguredRange(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse))
	store, openErr := statestore.Open(ctx, statestore.Options{
		Path:        storePath,
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	defer func() {
		require.NoError(t, store.Close())
	}()

	owner, ownerErr := process.This()
	require.NoError(t, ownerErr)
	ConfigureStateStorePortAllocator(store, owner)
	defer ConfigureStateStorePortAllocator(nil, process.ProcessHandle{})
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStore)
	t.Setenv(DCP_PORT_ALLOCATION_RANGE, "26010-26020")

	port1, port1Err := GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)
	port2, port2Err := GetFreePort(ctx, commonapi.PortProtocolTCP, IPv4LocalhostDefaultAddress, log)

	require.NoError(t, port1Err)
	require.NoError(t, port2Err)
	require.NotEqual(t, port1, port2)
	require.GreaterOrEqual(t, port1, int32(26010))
	require.LessOrEqual(t, port1, int32(26020))
	require.GreaterOrEqual(t, port2, int32(26010))
	require.LessOrEqual(t, port2, int32(26020))
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
