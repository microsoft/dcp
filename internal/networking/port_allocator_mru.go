/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	std_slices "slices"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/pkg/osutil"
)

func allocateRandomEphemeralPortWithMruFile(ctx context.Context, protocol apiv1.PortProtocol, address string, log logr.Logger) (int32, error) {
	portFileLock.Lock()
	portFileErr := ensurePackageMruPortFile(log)
	portFileLock.Unlock()
	var allocatedPort int32 = 0

	bestEffortPortAllocation := func(err error) (int32, error) {
		if packageMruPortFile.params.failOnPortFileError {
			return 0, err
		} else {
			return getRandomEphemeralPort(protocol, address)
		}
	}

	if portFileErr != nil {
		// Error will be logged by ensurePackageMruPortFile()
		log.V(1).Info("Warning: port allocation check: could not check the most recently used ports file for conflicts")
		return bestEffortPortAllocation(portFileErr)
	}

	mruPortsAllocCtx, cancel := context.WithTimeout(ctx, packageMruPortFile.params.portAllocationTimeout)
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
				log.Error(unlockErr, "Could not unlock the most recently used ports file") // Should never happen
				if packageMruPortFile.params.failOnPortFileError {
					panic(unlockErr)
				}
			}
		}()

		for i := 0; i < packageMruPortFile.params.portAllocationsPerRound; i++ {
			port, portErr := getRandomEphemeralPort(protocol, address)
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
				if writeErr := packageMruPortFile.WriteAndUnlock(ctx, usedPorts); writeErr != nil {
					log.Error(writeErr, "Could not write to the most recently used ports file")
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
		log.V(1).Info("Warning: port allocation check: could not check the most recently used ports file for conflicts",
			"Error", pollWaitErr.Error())
		return bestEffortPortAllocation(pollWaitErr)
	}
}

func checkPortAvailableWithMruFile(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) error {
	portFileLock.Lock()
	defer portFileLock.Unlock()

	portFileErr := ensurePackageMruPortFile(log)
	bestEffortCheck := func(err error) error {
		if packageMruPortFile.params.failOnPortFileError {
			return err
		} else {
			// Do best-effort check without considering the most recently used ports
			return checkPortCurrentlyBindable(protocol, address, port)
		}
	}

	if portFileErr != nil {
		// Error will be logged by ensurePackageMruPortFile()
		return bestEffortCheck(portFileErr)
	}

	// Check if the port was allocated by one of the DCP instances
	mruPortsCheckCtx, cancel := context.WithTimeout(ctx, packageMruPortFile.params.portAvailableCheckTimeout)
	defer cancel()

	usedPorts, usedPortsErr := packageMruPortFile.tryLockAndRead(mruPortsCheckCtx)
	if usedPortsErr != nil {
		log.V(1).Info("Warning: port availability check: could not check the most recently used ports file for conflicts", "Error", usedPortsErr.Error())
		return bestEffortCheck(usedPortsErr)
	}

	// Since we are not going to write anything to the file, we can unlock it immediately
	unlockErr := packageMruPortFile.Unlock()
	if unlockErr != nil {
		log.Error(unlockErr, "Could not unlock the most recently used ports file") // Should never happen
	}

	desired := AddressAndPort(address, port)
	i, found := std_slices.BinarySearchFunc(usedPorts, desired, matchAddressAndPort)
	if found && usedPorts[i].Instance != programInstanceID {
		return fmt.Errorf("port %d is already in use by another process", port)
	} else {
		return checkPortCurrentlyBindable(protocol, address, port)
	}
}

func releaseSpecificPortWithMruFile(ctx context.Context, address string, port int32, log logr.Logger) error {
	portFileLock.Lock()
	defer portFileLock.Unlock()

	portFileErr := ensurePackageMruPortFile(log)
	params := defaultMruPortFileUsageParameters()
	if packageMruPortFile != nil {
		params = packageMruPortFile.params
	}
	bestEffortRelease := func(err error) error {
		if params.failOnPortFileError {
			return err
		}
		return nil
	}

	if portFileErr != nil {
		// Error will be logged by ensurePackageMruPortFile()
		return bestEffortRelease(portFileErr)
	}

	mruPortsReleaseCtx, cancel := context.WithTimeout(ctx, packageMruPortFile.params.portAvailableCheckTimeout)
	defer cancel()

	usedPorts, usedPortsErr := packageMruPortFile.tryLockAndRead(mruPortsReleaseCtx)
	if usedPortsErr != nil {
		log.V(1).Info("Warning: port release: could not check the most recently used ports file for conflicts", "Error", usedPortsErr.Error())
		return bestEffortRelease(usedPortsErr)
	}

	desired := AddressAndPort(address, port)
	filteredPorts := usedPorts[:0]
	released := false
	for _, usedPort := range usedPorts {
		if usedPort.AddressAndPort == desired && usedPort.Instance == programInstanceID {
			released = true
			continue
		}
		filteredPorts = append(filteredPorts, usedPort)
	}

	if !released {
		unlockErr := packageMruPortFile.Unlock()
		if unlockErr != nil {
			log.Error(unlockErr, "Could not unlock the most recently used ports file")
			return bestEffortRelease(unlockErr)
		}
		return nil
	}

	writeErr := packageMruPortFile.WriteAndUnlock(mruPortsReleaseCtx, filteredPorts)
	if writeErr != nil {
		log.Error(writeErr, "Could not write to the most recently used ports file")
		return bestEffortRelease(writeErr)
	}
	return nil
}

func checkPortCurrentlyBindable(protocol apiv1.PortProtocol, address string, port int32) error {
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

func getRandomEphemeralPort(protocol apiv1.PortProtocol, address string) (int32, error) {
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

// Used by tests, makes the use of the most recently used ports file mandatory.
func EnableStrictMruPortHandling(log logr.Logger) {
	err := ensurePackageMruPortFile(log)
	if err != nil {
		panic("could not create the most recently used ports file; network port conflicts may occur")
	}
	packageMruPortFile.params.failOnPortFileError = true
}

// Creates the (default) most-recently-used ports file.
// Assumes mruPortFileLock is held (this function is not goroutine-safe).
func ensurePackageMruPortFile(log logr.Logger) error {
	if packageMruPortFile != nil {
		return nil
	}

	err := func() error {
		dcpFolder, dcpFolderErr := dcppaths.EnsureUserDcpDir()
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
		packageMruPortFile, creationErr = newMruPortFile(lockfilePath, defaultMruPortFileUsageParameters())
		return creationErr
	}()

	if err != nil && !portFileErrorReported {
		portFileErrorReported = true
		log.Error(err, "Could not create the most recently used ports file; network port conflicts may occur")
	}

	return err
}
