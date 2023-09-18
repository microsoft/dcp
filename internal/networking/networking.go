// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"fmt"
	"net"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(protocol apiv1.PortProtocol, address string) (int32, error) {
	if address == "" {
		address = "localhost"
	}

	if protocol == apiv1.UDP {
		udpaddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", address))
		if err != nil {
			return 0, err
		}

		if listener, err := net.ListenUDP("udp", udpaddr); err != nil {
			return 0, err
		} else {
			port := int32(listener.LocalAddr().(*net.UDPAddr).Port)
			listener.Close()
			return port, nil
		}
	} else {
		tcpaddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", address))
		if err != nil {
			return 0, err
		}

		if listener, err := net.ListenTCP("tcp", tcpaddr); err != nil {
			return 0, err
		} else {
			port := int32(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()
			return port, nil
		}
	}
}
