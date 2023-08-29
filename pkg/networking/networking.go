// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"fmt"
	"net"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

func GetFreePort(protocol apiv1.PortProtocol, address string) (uint16, error) {
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
			port := uint16(listener.LocalAddr().(*net.UDPAddr).Port)
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
			port := uint16(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()
			return port, nil
		}
	}
}
