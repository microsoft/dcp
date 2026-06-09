/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/nettest"

	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/slices"
)

type IpVersionPreference string

const (
	instanceIdLength uint32 = 12

	IpVersionPreferenceNone IpVersionPreference = "None"
	IpVersionPreference4    IpVersionPreference = "IPv4"
	IpVersionPreference6    IpVersionPreference = "IPv6"

	Localhost                   = "localhost"
	IPv4LocalhostDefaultAddress = "127.0.0.1"
	IPv6LocalhostDefaultAddress = "[::1]"
	IPv4AllInterfaceAddress     = "0.0.0.0" // Equivalent to net.IPv4zero
	IPv6AllInterfaceAddress     = "[::]"    // Equivalent to net.IPv6zero

	InvalidPort = 0

	// Use a relatively short timeout for network operations such as IP lookup or socket open
	// because we are only interested in local addresses and ports, so the operation should be fast.
	NetworkOpTimeout = 2 * time.Second

	DCP_INSTANCE_ID_PREFIX = "DCP_INSTANCE_ID_PREFIX"

	DCP_PORT_ALLOCATOR                          = "DCP_PORT_ALLOCATOR"
	DCP_PORT_ALLOCATION_RANGE                   = "DCP_PORT_ALLOCATION_RANGE"
	DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP = "DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP"

	portAllocatorModeMru                    = "mru"
	portAllocatorModeStateStore             = "statestore"
	portAllocatorModeStateStoreWithFallback = "statestore-with-mru-fallback"
)

var (
	portFileLock *sync.Mutex
	// portAllocatorLock protects quick package-level allocator configuration reads/writes.
	portAllocatorLock         *sync.Mutex
	portFileErrorReported     bool
	portRangeOverlapReported  bool
	packageMruPortFile        *mruPortFile
	packageStateStore         *statestore.Store
	packageStateStoreOwner    process.ProcessTreeItem
	programInstanceID         string
	ipVersionPreference       IpVersionPreference
	getAllLocalIpsOnce        func() ([]net.IP, error)
	getHostnameOnce           func() (string, error)
	getEphemeralPortRangeOnce func() (portRange, bool)
)

type portRange struct {
	Start int
	End   int
}

func init() {
	portFileLock = &sync.Mutex{}
	portAllocatorLock = &sync.Mutex{}
	getAllLocalIpsOnce = sync.OnceValues(getAllLocalIps)
	getHostnameOnce = sync.OnceValues(os.Hostname)
	getEphemeralPortRangeOnce = sync.OnceValues(getEphemeralPortRange)

	idBytes, err := randdata.MakeRandomString(instanceIdLength)
	if err != nil {
		panic("failed to create DCP instance ID: " + err.Error())
	}
	programInstanceID = string(idBytes)
	value, found := os.LookupEnv(DCP_INSTANCE_ID_PREFIX)
	if found && strings.TrimSpace(value) != "" {
		programInstanceID = value + programInstanceID
	}

	value, found = os.LookupEnv("DCP_IP_VERSION_PREFERENCE")
	if !found {
		ipVersionPreference = IpVersionPreferenceNone
	} else {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "ipv4", "v4", "4":
			ipVersionPreference = IpVersionPreference4
		case "ipv6", "v6", "6":
			ipVersionPreference = IpVersionPreference6
		default:
			ipVersionPreference = IpVersionPreferenceNone
		}
	}
}

func ConfigureStateStorePortAllocator(store *statestore.Store, owner process.ProcessTreeItem) {
	portAllocatorLock.Lock()
	defer portAllocatorLock.Unlock()

	packageStateStore = store
	packageStateStoreOwner = owner
}

// Wrap the standard net.LookupIP method to filter for supported IP address types
func LookupIP(host string) ([]net.IP, error) {
	var validIps []net.IP

	// The LookupIP method will not resolve 0.0.0.0 or [::] to a set of specific interface addresses, so we need to do it "manually".
	switch host {
	case IPv4AllInterfaceAddress:
		allLocalIps, localIpsErr := getAllLocalIpsOnce()
		if localIpsErr != nil {
			return nil, localIpsErr
		}
		validIps = slices.Select(allLocalIps, IsValidIPv4)

	case IPv6AllInterfaceAddress:
		allLocalIps, localIpsErr := getAllLocalIpsOnce()
		if localIpsErr != nil {
			return nil, localIpsErr
		}
		validIps = slices.Select(allLocalIps, IsValidIPv6)

	default:
		host = ToStandaloneAddress(host) // LookupIP does not like brackets around IPv6 addresses
		ctx, cancel := context.WithTimeout(context.Background(), NetworkOpTimeout)
		defer cancel()

		// Ensure that any address that ends with ".localhost" is resolved as "localhost"
		// Not all DNS servers resolve *.localhost names to loopback addresses correctly
		if strings.HasSuffix(strings.ToLower(host), ".localhost") {
			host = Localhost
		}

		switch host {
		case Localhost:
			// If this is localhost, resolve it to all loopback addresses
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}

			validIps = slices.Select(ips, IsValidIP)
		default:
			if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
				// This is a direct IP address, so we attempt to use it as is
				validIps = slices.Select([]net.IP{netIPFromAddr(addr)}, IsValidIP)
			} else {
				// This is an arbitrary hostname, so resolve it to all local IP addresses
				allLocalIps, err := getAllLocalIpsOnce()
				if err != nil {
					return nil, err
				}

				validIps = allLocalIps
			}
		}
	}

	return validIps, nil
}

func getAllLocalIps() ([]net.IP, error) {
	ifaces, ifaceErr := net.Interfaces()
	if ifaceErr != nil {
		return nil, ifaceErr
	}

	var candidateIps []net.IP

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue // Very unlikely to fail if net.Interfaces() succeeded
		}

		for _, addr := range addrs {
			switch ipaddr := addr.(type) {
			case *net.IPNet:
				if IsValidIP(ipaddr.IP) {
					candidateIps = append(candidateIps, ipaddr.IP)
				}
			case *net.IPAddr:
				// Note: if we ever need to support link-local addresses, we would need to include the zone identifier
				// (net.IPAddr.Zone) in the final address. Currently we filter out link-local addresses in IsValidIP() function,
				// so that is not an issue.
				if IsValidIP(ipaddr.IP) {
					candidateIps = append(candidateIps, ipaddr.IP)
				}
			}
		}
	}

	// Make sure these addresses are usable by trying to bind to them
	var verifiedIps []net.IP
	for _, ip := range candidateIps {
		lc := net.ListenConfig{}
		ctx, cancel := context.WithTimeout(context.Background(), NetworkOpTimeout)
		listener, listenerErr := lc.Listen(ctx, "tcp", AddressAndPort(IpToString(ip), 0))
		cancel()
		if listenerErr == nil {
			listener.Close()
			verifiedIps = append(verifiedIps, ip)
		}
	}

	// If we bind to all interfaces, prefer the routable, non-loopback addresses,
	// then non-routable, non-loopback addresses, and finally loopback addresses.
	allLocalIps := append(
		slices.Select(verifiedIps, func(ip net.IP) bool { return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() }),
		slices.Select(verifiedIps, func(ip net.IP) bool { return !ip.IsLoopback() && ip.IsLinkLocalUnicast() })...,
	)
	allLocalIps = append(allLocalIps, slices.Select(verifiedIps, net.IP.IsLoopback)...)
	return allLocalIps, nil
}

func GetEphemeralPortRange() (int, int, bool) {
	ephemeralRange, matched := getEphemeralPortRangeOnce()
	return ephemeralRange.Start, ephemeralRange.End, matched
}

// NormalizePortAllocationAddress converts a caller-provided bind address into the concrete address
// used by the port allocator.
func NormalizePortAllocationAddress(address string) (string, error) {
	if address == "" || address == Localhost {
		preferredIps, preferredErr := GetPreferredHostIps(Localhost)
		if preferredErr != nil {
			return "", preferredErr
		}
		preferredAddr, ok := addrFromNetIP(preferredIps[0])
		if !ok {
			return "", fmt.Errorf("could not parse preferred localhost IP address: %s", preferredIps[0].String())
		}
		return addrToAddressString(preferredAddr), nil
	}
	addr, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	if parseErr != nil {
		// We treat any non-Localhost and non-IP address as a request to bind to all interfaces.
		return IPv4AllInterfaceAddress, nil
	}
	return addrToAddressString(addr), nil
}

func IpToString(ip net.IP) string {
	addr, ok := addrFromNetIP(ip)
	if !ok {
		return ip.String()
	}
	return addrToAddressString(addr)
}

func IsIPv4(address string) bool {
	addr, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	return parseErr == nil && addr.Unmap().Is4() && nettest.SupportsIPv4()
}

func IsIPv6(address string) bool {
	addr, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	return parseErr == nil && addr.Is6() && !addr.Is4In6() && nettest.SupportsIPv6()
}

func addrFromNetIP(ip net.IP) (netip.Addr, bool) {
	ip4 := ip.To4()
	if ip4 != nil {
		var bytes [4]byte
		copy(bytes[:], ip4)
		return netip.AddrFrom4(bytes), true
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return netip.Addr{}, false
	}
	var bytes [16]byte
	copy(bytes[:], ip16)
	return netip.AddrFrom16(bytes), true
}

func netIPFromAddr(addr netip.Addr) net.IP {
	addr = addr.Unmap()
	if addr.Is4() {
		bytes := addr.As4()
		return net.IPv4(bytes[0], bytes[1], bytes[2], bytes[3])
	}
	bytes := addr.As16()
	return append(net.IP(nil), bytes[:]...)
}

func addrToAddressString(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return fmt.Sprintf("[%s]", addr.String())
	}
	return addr.String()
}

// Removes the brackets from an IPv6 address if they are present.
//
// Note: in most cases an IPv6 address needs to be enclosed in brackets for disambiguation
// (e.g. inside URLs, whenever a port number follows). This is why inside DCP we mostly stick
// to bracketed IPv6 addresses. But certain APIs require a standalone address, i.e. without brackets.
func ToStandaloneAddress(address string) string {
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		return address[1 : len(address)-1]
	} else {
		return address
	}
}

func IsValidPort(port int) bool {
	return ports.IsValidPort(port)
}

func IsBindablePort(port int) bool {
	return ports.IsBindablePort(port)
}

func IsEphemeralPort(port int32) bool {
	start, end, _ := GetEphemeralPortRange()
	return int(port) >= start && int(port) <= end
}

func IsValidIP(ip net.IP) bool {
	return (IsValidIPv4(ip) || IsValidIPv6(ip)) &&
		!ip.Equal(net.IPv4zero) && !ip.Equal(net.IPv6zero) &&
		!ip.Equal(net.IPv4bcast) && !ip.IsMulticast() &&
		// Link-local addresses are difficult to bind to (because of mandatory zone identifier)
		// and do not add any value over normal loopback addresses
		!ip.IsLinkLocalUnicast()
}

func IsValidIPv4(ip net.IP) bool {
	return len(ip.To4()) == net.IPv4len && nettest.SupportsIPv4()
}

func IsValidIPv6(ip net.IP) bool {
	// ip.To16() always works (it is always possible to convert IPv4 address to IPv6)
	// But we are not interested in conversion; we want to know if the passed address is native IPv6 and not IPv4.
	// This is why we check if IPv4 conversion fails.
	return len(ip.To16()) == net.IPv6len && len(ip.To4()) == 0 && nettest.SupportsIPv6()
}

func GetProgramInstanceID() string {
	return programInstanceID
}

func GetIpVersionPreference() IpVersionPreference {
	return ipVersionPreference
}

func AddressAndPort(address string, port int32) string {
	return fmt.Sprintf("%s:%d", address, port)
}

func Hostname() (string, error) {
	// Returns the hostname of the machine, which is used for address resolution.
	// This is a wrapper around os.Hostname() to ensure that it is only called once.
	return getHostnameOnce()
}

func GetPreferredHostIps(host string) ([]net.IP, error) {
	// Some host names are pseudo-addresses that resolve to multiple IP addresses.
	// For example, "localhost"  hostname resolves to all loopback addresses on the machine. For dual-stack machines (very common)
	// it will contain both IPv4 and IPv6 addresses. However, different programming languages and libraries may
	// "choose" different addresses to try first (e.g. some might prefer IPv4 vs IPv6).
	// The result can be long connection delays.
	// A similar problem can occur with "0.0.0.0" address. That is good for listening
	// (it causes to listen on all available network interfaces), but it is not something that a client can use to talk to a server.
	// To avoid these problems we resolve host names/IPs specified in object Spec/annotations,
	// and use specific IP addresses for configuring proxies and clients, as necessary.

	ips, err := LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("could not obtain IP address(es) for '%s': %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("could not obtain IP address(es) for '%s' (no valid addresses found)", host)
	}

	// Account for IP version user preference if possible
	var isPreferredIp func(net.IP) bool
	switch GetIpVersionPreference() {
	case IpVersionPreference4:
		isPreferredIp = IsValidIPv4
	case IpVersionPreference6:
		isPreferredIp = IsValidIPv6
	default:
		isPreferredIp = func(_ net.IP) bool { return true }
	}
	preferredIps := slices.Select(ips, isPreferredIp)
	if len(preferredIps) > 0 {
		ips = preferredIps
	}

	return ips, nil
}
