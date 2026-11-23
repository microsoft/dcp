/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"fmt"
	"net"
	"strings"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// SelectServiceAddress selects a usable address/port pair without modifying addresses.
// requiredFamily filters candidates; preferredFamily ranks them without preventing fallback.
// Empty/None means no filter and IPv4 preference respectively. Ties use address then port ordering.
// If an explicit family has no published matching IP, local host addresses are resolved using LookupIP.
// The boolean is false when no usable address matches the filter.
func SelectServiceAddress(
	addresses []apiv2.ServiceAddress, requiredFamily IpVersionPreference, preferredFamily IpVersionPreference,
) (apiv2.ServiceAddress, bool, error) {
	switch requiredFamily {
	case "", IpVersionPreferenceNone, IpVersionPreference4, IpVersionPreference6:
	default:
		return apiv2.ServiceAddress{}, false, fmt.Errorf("unsupported required address family: %s", requiredFamily)
	}
	switch preferredFamily {
	case "", IpVersionPreferenceNone, IpVersionPreference4, IpVersionPreference6:
	default:
		return apiv2.ServiceAddress{}, false, fmt.Errorf("unsupported preferred address family: %s", preferredFamily)
	}

	selected, found := selectPublishedServiceAddress(addresses, requiredFamily, preferredFamily)
	if found || requiredFamily == "" || requiredFamily == IpVersionPreferenceNone {
		return selected, found, nil
	}

	var resolved []apiv2.ServiceAddress
	for _, address := range addresses {
		if address.Address == "" || !IsValidPort(int(address.Port)) ||
			net.ParseIP(ToStandaloneAddress(address.Address)) != nil {
			continue
		}
		ips, lookupErr := LookupIP(address.Address)
		if lookupErr != nil {
			return apiv2.ServiceAddress{}, false, fmt.Errorf("resolve local service address %q: %w", address.Address, lookupErr)
		}
		for _, ip := range ips {
			resolved = append(resolved, apiv2.ServiceAddress{Address: IpToString(ip), Port: address.Port})
		}
	}
	selected, found = selectPublishedServiceAddress(resolved, requiredFamily, preferredFamily)
	return selected, found, nil
}

func selectPublishedServiceAddress(
	addresses []apiv2.ServiceAddress, requiredFamily IpVersionPreference, preferredFamily IpVersionPreference,
) (apiv2.ServiceAddress, bool) {
	var selected apiv2.ServiceAddress
	found, selectedPreferred := false, false
	for _, address := range addresses {
		if requiredFamily == IpVersionPreference4 && !IsIPv4(address.Address) ||
			requiredFamily == IpVersionPreference6 && !IsIPv6(address.Address) ||
			address.Address == "" || !IsValidPort(int(address.Port)) {
			continue
		}
		preferred := IsIPv4(address.Address)
		if preferredFamily == IpVersionPreference6 {
			preferred = IsIPv6(address.Address)
		}
		addressOrder := strings.Compare(address.Address, selected.Address)
		if !found || preferred && !selectedPreferred ||
			preferred == selectedPreferred && (addressOrder < 0 || addressOrder == 0 && address.Port < selected.Port) {
			selected, selectedPreferred, found = address, preferred, true
		}
	}
	return selected, found
}
