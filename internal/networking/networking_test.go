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

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
)

type protocolTestCase struct {
	name     string
	protocol apiv1.PortProtocol
}

var (
	protocolTestCases = []protocolTestCase{
		{
			name:     "TCP",
			protocol: apiv1.TCP,
		},
		{
			name:     "UDP",
			protocol: apiv1.UDP,
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

					if tc.protocol == apiv1.UDP {
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
	for _, candidateRange := range ranges {
		require.True(t, candidateRange.End < ephemeralStart || candidateRange.Start > ephemeralEnd)
	}
}

func TestPortAllocationCandidatesCoverRange(t *testing.T) {
	t.Parallel()

	ranges := []portRange{
		{Start: 26010, End: 26011},
		{Start: 26020, End: 26022},
	}

	candidateIterator := newStateStorePortAllocationCandidateIterator(ranges, string(apiv1.TCP), IPv4LocalhostDefaultAddress)
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

	address, err := normalizePortAllocationAddress(Localhost)

	require.NoError(t, err)
	require.NotEqual(t, Localhost, address)
	_, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	require.NoError(t, parseErr)
}

func TestNormalizePortAllocationAddressCanonicalizesIPs(t *testing.T) {
	t.Parallel()

	ipv4MappedAddress, ipv4MappedErr := normalizePortAllocationAddress("[::ffff:127.0.0.1]")
	require.NoError(t, ipv4MappedErr)
	require.Equal(t, IPv4LocalhostDefaultAddress, ipv4MappedAddress)

	ipv6Address, ipv6Err := normalizePortAllocationAddress("[0:0:0:0:0:0:0:1]")
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
	defer ConfigureStateStorePortAllocator(nil, process.ProcessTreeItem{})
	t.Setenv(DCP_PORT_ALLOCATOR, portAllocatorModeStateStore)
	t.Setenv(DCP_PORT_ALLOCATION_RANGE, "26010-26020")

	port1, port1Err := GetFreePort(ctx, apiv1.TCP, IPv4LocalhostDefaultAddress, log)
	port2, port2Err := GetFreePort(ctx, apiv1.TCP, IPv4LocalhostDefaultAddress, log)

	require.NoError(t, port1Err)
	require.NoError(t, port2Err)
	require.NotEqual(t, port1, port2)
	require.GreaterOrEqual(t, port1, int32(26010))
	require.LessOrEqual(t, port1, int32(26020))
	require.GreaterOrEqual(t, port2, int32(26010))
	require.LessOrEqual(t, port2, int32(26020))
}
