/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ports

import "net/netip"

// Binding identifies a protocol/IP/port tuple.
type Binding struct {
	Protocol string
	IP       netip.Addr
	Port     int32
}

func IsValidPort(port int) bool {
	return port >= 1 && port <= 65535
}

func IsBindablePort(port int) bool {
	return port == 0 || IsValidPort(port)
}
