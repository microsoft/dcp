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

		if listener, listenErr := net.ListenUDP("udp", udpaddr); listenErr != nil {
			return 0, listenErr
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

		if listener, listenErr := net.ListenTCP("tcp", tcpaddr); listenErr != nil {
			return 0, listenErr
		} else {
			port := int32(listener.Addr().(*net.TCPAddr).Port)
			listener.Close()
			return port, nil
		}
	}
}

func CheckPortAvailable(protocol apiv1.PortProtocol, address string, port int32) error {
	if address == "" {
		address = "localhost"
	}

	if protocol == apiv1.UDP {
		udpaddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", address, port))
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
		tcpaddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", address, port))
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
