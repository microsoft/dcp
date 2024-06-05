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

const (
	instanceIdLength uint32 = 12
)

var (
	portFileLock          *sync.Mutex
	portFileErrorReported bool
	packageMruPortFile    *mruPortFile
	programInstanceID     string
)

func init() {
	portFileLock = &sync.Mutex{}

	idBytes, err := randdata.MakeRandomString(instanceIdLength)
	if err != nil {
		panic("failed to create DCP instance ID: " + err.Error())
	}
	programInstanceID = string(idBytes)
	value, found := os.LookupEnv("DCP_INSTANCE_ID_PREFIX")
	if found && strings.TrimSpace(value) != "" {
		programInstanceID = value + programInstanceID
	}
}

// Wrap the standard net.LookupIP method to filter for supported IP address types
func LookupIP(host string) ([]net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	return slices.Select(ips, IsValidIP), nil
}

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(protocol apiv1.PortProtocol, address string, log logr.Logger) (int32, error) {
	if address == "" {
		address = "localhost"
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
		address = "localhost"
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
	// The order of checks is significant here
	if ip4 := ip.To4(); len(ip4) == net.IPv4len {
		address = ip4.String()
	} else if ip6 := ip.To16(); len(ip6) == net.IPv6len {
		address = fmt.Sprintf("[%s]", ip6.String())
	} else {
		// Not sure what kind address this is, but it is worth trying
		address = ip.String()
	}
	return address
}

func IsIPv4(address string) bool {
	return net.ParseIP(address).To4() != nil
}

func IsIPv6(address string) bool {
	return net.ParseIP(address).To16() != nil
}

func IsValidPort(port int) bool {
	return port >= 1 && port <= 65535
}

func IsValidIP(ip net.IP) bool {
	if ip.To4() != nil && nettest.SupportsIPv4() {
		return true
	} else if len(ip.To16()) == net.IPv6len && nettest.SupportsIPv6() {
		return true
	}

	return false
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
