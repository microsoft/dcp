// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	std_slices "slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/nettest"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
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

	// Use a relatively short timeout for network operations such as IP lookup or socket open
	// because we are only interested in local addresses and ports, so the operation should be fast.
	NetworkOpTimeout = 2 * time.Second
)

var (
	portFileLock          *sync.Mutex
	portFileErrorReported bool
	packageMruPortFile    *mruPortFile
	programInstanceID     string
	ipVersionPreference   IpVersionPreference
	getAllLocalIpsOnce    func() ([]net.IP, error)
)

func init() {
	portFileLock = &sync.Mutex{}
	getAllLocalIpsOnce = sync.OnceValues(getAllLocalIps)

	idBytes, err := randdata.MakeRandomString(instanceIdLength)
	if err != nil {
		panic("failed to create DCP instance ID: " + err.Error())
	}
	programInstanceID = string(idBytes)
	value, found := os.LookupEnv("DCP_INSTANCE_ID_PREFIX")
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
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}

		validIps = slices.Select(ips, IsValidIP)
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

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(protocol apiv1.PortProtocol, address string, log logr.Logger) (int32, error) {
	if address == "" {
		address = Localhost
	}

	portFileLock.Lock()
	portFileErr := ensurePackageMruPortFile(log)
	portFileLock.Unlock()
	var allocatedPort int32 = 0

	bestEffortPortAllocation := func(err error) (int32, error) {
		if packageMruPortFile.params.failOnPortFileError {
			return 0, err
		} else {
			return doGetFreePort(protocol, address)
		}
	}

	if portFileErr != nil {
		// Error will be logged by ensurePackageMruPortFile()
		log.V(1).Info("warning: port allocation check: could not check the most recently used ports file for conflicts")
		return bestEffortPortAllocation(portFileErr)
	}

	mruPortsAllocCtx, cancel := context.WithTimeout(context.Background(), packageMruPortFile.params.portAllocationTimeout)
	defer cancel()

	pollWaitErr := wait.PollUntilContextCancel(mruPortsAllocCtx, packageMruPortFile.params.portAllocationRoundDelay, true /* poll immediately */, func(ctx context.Context) (bool, error) {
		portFileLock.Lock()
		defer portFileLock.Unlock()

		usedPorts, usedPortsErr := packageMruPortFile.tryLockAndRead(mruPortsAllocCtx)
		if usedPortsErr != nil {
			return false, usedPortsErr
		}
		defer func() {
			if unlockErr := packageMruPortFile.Unlock(); unlockErr != nil {
				log.Error(unlockErr, "could not unlock the most recently used ports file") // Should never happen
				if packageMruPortFile.params.failOnPortFileError {
					panic(unlockErr)
				}
			}
		}()

		for i := 0; i < packageMruPortFile.params.portAllocationsPerRound; i++ {
			port, portErr := doGetFreePort(protocol, address)
			if portErr != nil {
				return false, portErr
			}

			_, recentlyUsed := std_slices.BinarySearchFunc(usedPorts, AddressAndPort(address, port), matchAddressAndPort)

			if !recentlyUsed {
				allocatedPort = port
				usedPorts = append(usedPorts, mruPortFileRecord{
					Address:        address,
					Port:           allocatedPort,
					AddressAndPort: AddressAndPort(address, allocatedPort),
					Timestamp:      time.Now(),
					Instance:       programInstanceID,
				})
				if writeErr := packageMruPortFile.writeAndUnlock(ctx, usedPorts); writeErr != nil {
					log.Error(writeErr, "could not write to the most recently used ports file")
					if packageMruPortFile.params.failOnPortFileError {
						return false, writeErr
					}
				}
				return true, nil
			}
		}

		return false, nil // Keep trying
	})

	if pollWaitErr == nil {
		return allocatedPort, nil
	} else {
		log.V(1).Info("warning: port allocation check: could not check the most recently used ports file for conflicts",
			"error", pollWaitErr.Error())
		return bestEffortPortAllocation(pollWaitErr)
	}
}

func CheckPortAvailable(protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) error {
	if address == "" {
		address = Localhost
	}

	portFileLock.Lock()
	defer portFileLock.Unlock()

	portFileErr := ensurePackageMruPortFile(log)
	bestEffortCheck := func(err error) error {
		if packageMruPortFile.params.failOnPortFileError {
			return err
		} else {
			// Do best-effort check without considering the most recently used ports
			return doCheckPortAvailable(protocol, address, port)
		}
	}

	if portFileErr != nil {
		// Error will be logged by ensurePackageMruPortFile()
		return bestEffortCheck(portFileErr)
	}

	// Check if the port was allocated by one of the DCP instances
	mruPortsCheckCtx, cancel := context.WithTimeout(context.Background(), packageMruPortFile.params.portAvailableCheckTimeout)
	defer cancel()

	usedPorts, usedPortsErr := packageMruPortFile.tryLockAndRead(mruPortsCheckCtx)
	if usedPortsErr != nil {
		log.V(1).Info("warning: port availability check: could not check the most recently used ports file for conflicts", "error", usedPortsErr.Error())
		return bestEffortCheck(usedPortsErr)
	}

	// Since we are not going to write anyting to the file, we can unlock it immediately
	unlockErr := packageMruPortFile.Unlock()
	if unlockErr != nil {
		log.Error(unlockErr, "could not unlock the most recently used ports file") // Should never happen
	}

	desired := AddressAndPort(address, port)
	i, found := std_slices.BinarySearchFunc(usedPorts, desired, matchAddressAndPort)
	if found && usedPorts[i].Instance != programInstanceID {
		return fmt.Errorf("port %d is already in use by another process", port)
	} else {
		return doCheckPortAvailable(protocol, address, port)
	}
}

func IpToString(ip net.IP) string {
	var address string
	if IsValidIPv6(ip) {
		address = fmt.Sprintf("[%s]", ip.String())
	} else {
		address = ip.String()
	}
	return address
}

func IsIPv4(address string) bool {
	return IsValidIPv4(net.ParseIP(ToStandaloneAddress(address)))
}

func IsIPv6(address string) bool {
	return IsValidIPv6(net.ParseIP(ToStandaloneAddress(address)))
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
	return port >= 1 && port <= 65535
}

func IsValidIP(ip net.IP) bool {
	return (IsValidIPv4(ip) || IsValidIPv6(ip)) &&
		!ip.Equal(net.IPv4zero) && !ip.Equal(net.IPv6zero) &&
		!ip.Equal(net.IPv4bcast) && !ip.IsMulticast()
}

func IsValidIPv4(ip net.IP) bool {
	return len(ip.To4()) == net.IPv4len && nettest.SupportsIPv4()
}

func IsValidIPv6(ip net.IP) bool {
	// ip.To16() always works (it is always possible to convert IPv4 adress to IPv6)
	// But we are not interested in conversion; we want to know if the passed address is native IPv6 and not IPv4.
	// This is why we check if IPv4 conversion fails.
	return len(ip.To16()) == net.IPv6len && len(ip.To4()) == 0 && nettest.SupportsIPv6()
}

// Used by tests, makes the use of the most recently used ports file mandatory
func EnableStrictMruPortHandling(log logr.Logger) {
	err := ensurePackageMruPortFile(log)
	if err != nil {
		panic("could not create the most recently used ports file; network port conflicts may occur")
	}
	packageMruPortFile.params.failOnPortFileError = true
}

func GetProgramInstanceID() string {
	return programInstanceID
}

func GetIpVersionPreference() IpVersionPreference {
	return ipVersionPreference
}

func doCheckPortAvailable(protocol apiv1.PortProtocol, address string, port int32) error {
	if protocol == apiv1.UDP {
		udpaddr, err := net.ResolveUDPAddr("udp", AddressAndPort(address, port))
		if err != nil {
			return err
		}

		if listener, listenErr := net.ListenUDP("udp", udpaddr); listenErr != nil {
			return listenErr
		} else {
			listener.Close()
			return nil
		}
	} else {
		tcpaddr, err := net.ResolveTCPAddr("tcp", AddressAndPort(address, port))
		if err != nil {
			return err
		}

		if listener, listenErr := net.ListenTCP("tcp", tcpaddr); listenErr != nil {
			return listenErr
		} else {
			listener.Close()
			return nil
		}
	}
}

func doGetFreePort(protocol apiv1.PortProtocol, address string) (int32, error) {
	if protocol == apiv1.UDP {
		udpaddr, err := net.ResolveUDPAddr("udp", AddressAndPort(address, 0))
		if err != nil {
			return 0, err
		}

		if listener, listenErr := net.ListenUDP("udp", udpaddr); listenErr != nil {
			return 0, listenErr
		} else {
			port := int32(listener.LocalAddr().(*net.UDPAddr).Port)
			listener.Close()
			return port, nil
		}
	} else {
		tcpaddr, err := net.ResolveTCPAddr("tcp", AddressAndPort(address, 0))
		if err != nil {
			return 0, err
		}

		if listener, listenErr := net.ListenTCP("tcp", tcpaddr); listenErr != nil {
			return 0, listenErr
		} else {
			port := int32(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()
			return port, nil
		}
	}
}

func AddressAndPort(address string, port int32) string {
	return fmt.Sprintf("%s:%d", address, port)
}

// Creates the (default) most-recently-used ports file.
// Assumes mruPortFileLock is held (this function is not goroutine-safe).
func ensurePackageMruPortFile(log logr.Logger) error {
	if packageMruPortFile != nil {
		return nil
	}

	err := func() error {
		dcpFolder, dcpFolderErr := dcppaths.EnsureDcpRootDir()
		if dcpFolderErr != nil {
			return dcpFolderErr
		}

		isAdmin, isAdminErr := osutil.IsAdmin()
		if isAdminErr != nil {
			return isAdminErr
		}

		var lockfilePath string
		if isAdmin {
			lockfilePath = filepath.Join(dcpFolder, "mruPorts.elevated.list")
		} else {
			lockfilePath = filepath.Join(dcpFolder, "mruPorts.list")
		}

		var creationErr error
		packageMruPortFile, creationErr = NewMruPortFile(lockfilePath, defaultMruPortFileUsageParameters())
		return creationErr
	}()

	if err != nil && !portFileErrorReported {
		portFileErrorReported = true
		log.Error(err, "could not create the most recently used ports file; network port conflicts may occur")
	}

	return err
}

func GetPreferredHostIps(host string) ([]net.IP, error) {
	// Some host names are pseudo-addresses that resolve to multiple IP addresses.
	// For example, "localhost"  hostname resolves to all loopback addresses on the machine. For dual-stack machines (very common)
	// it will contain both IPv4 and IPv6 addresses. However, different programming languages and libraries may
	// "choose" different addresses to try first (e.g. some might prefer IPv4 vs IPv6).
	// The result can be long connection delays.
	// A similar problem can occur with "0.0.0.0" address. That is good for listening
	// (it causes to listen on all available network interfaces), but it is not something that a client can use to talk to a server.
	// To avoid these problems we resolve host names/IPs specified in object Spec/annotaions,
	// and use specific IP addresses for configuring proxies and clients, as necessary.

	ips, err := LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("could not obtain IP address(es) for '%s': %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("could not obtain IP address(es) for '%s' (no valid addresses found)", host)
	}

	// Account for IP version user preference if possble
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
