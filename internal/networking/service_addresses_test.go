/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"net"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// Verifies that SelectServiceAddress filters required families, ranks preferred families with
// fallback, ignores unusable pairs, and selects deterministically without modifying its input.
func TestSelectServiceAddress(t *testing.T) {
	t.Parallel()
	ipv4 := apiv2.ServiceAddress{Address: "127.0.0.1", Port: 5000}
	ipv6 := apiv2.ServiceAddress{Address: "[::1]", Port: 5001}
	hostname := apiv2.ServiceAddress{Address: "localhost", Port: 5002}
	for _, testCase := range []struct {
		name      string
		addresses []apiv2.ServiceAddress
		required  IpVersionPreference
		preferred IpVersionPreference
		expected  apiv2.ServiceAddress
		found     bool
		needsIPv6 bool
	}{
		{name: "default prefers IPv4", addresses: []apiv2.ServiceAddress{ipv6, ipv4}, expected: ipv4, found: true},
		{name: "None prefers IPv4", addresses: []apiv2.ServiceAddress{ipv6, ipv4}, required: IpVersionPreferenceNone, preferred: IpVersionPreferenceNone, expected: ipv4, found: true},
		{name: "prefer IPv6", addresses: []apiv2.ServiceAddress{ipv4, ipv6}, preferred: IpVersionPreference6, expected: ipv6, found: true, needsIPv6: true},
		{name: "preferred family falls back", addresses: []apiv2.ServiceAddress{ipv4}, preferred: IpVersionPreference6, expected: ipv4, found: true},
		{name: "require IPv4", addresses: []apiv2.ServiceAddress{ipv6, ipv4}, required: IpVersionPreference4, preferred: IpVersionPreference6, expected: ipv4, found: true},
		{name: "require IPv6", addresses: []apiv2.ServiceAddress{ipv4, ipv6}, required: IpVersionPreference6, expected: ipv6, found: true, needsIPv6: true},
		{name: "required family cannot fall back", addresses: []apiv2.ServiceAddress{ipv4}, required: IpVersionPreference6},
		{name: "hostname without filter", addresses: []apiv2.ServiceAddress{hostname}, expected: hostname, found: true},
		{name: "localhost resolves to IPv4", addresses: []apiv2.ServiceAddress{hostname}, required: IpVersionPreference4,
			preferred: IpVersionPreference6, expected: apiv2.ServiceAddress{Address: "127.0.0.1", Port: hostname.Port}, found: true},
		{name: "localhost resolves to IPv6", addresses: []apiv2.ServiceAddress{hostname}, required: IpVersionPreference6,
			expected: apiv2.ServiceAddress{Address: "[::1]", Port: hostname.Port}, found: true, needsIPv6: true},
		{name: "published matching IP preferred to hostname resolution", addresses: []apiv2.ServiceAddress{
			hostname, {Address: "127.0.0.2", Port: 5003},
		}, required: IpVersionPreference4, expected: apiv2.ServiceAddress{Address: "127.0.0.2", Port: 5003}, found: true},
		{name: "IP preferred to hostname", addresses: []apiv2.ServiceAddress{hostname, ipv4}, expected: ipv4, found: true},
		{name: "bracketed IPv6 retained", addresses: []apiv2.ServiceAddress{{Address: "[::1]", Port: 5001}}, required: IpVersionPreference6, expected: apiv2.ServiceAddress{Address: "[::1]", Port: 5001}, found: true, needsIPv6: true},
		{name: "address tie break", addresses: []apiv2.ServiceAddress{{Address: "127.0.0.2", Port: 1}, ipv4}, expected: ipv4, found: true},
		{name: "port tie break", addresses: []apiv2.ServiceAddress{{Address: ipv4.Address, Port: 5003}, ipv4}, expected: ipv4, found: true},
		{name: "duplicates", addresses: []apiv2.ServiceAddress{ipv4, ipv4}, expected: ipv4, found: true},
		{name: "invalid pairs ignored", addresses: []apiv2.ServiceAddress{{Port: 5000}, {Address: "[::1]"}, {Address: "localhost", Port: -1}, {Address: "127.0.0.1", Port: 65536}, ipv4}, expected: ipv4, found: true},
		{name: "all pairs invalid", addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1"}, {Address: "[::1]", Port: 65536}}},
		{name: "empty", addresses: []apiv2.ServiceAddress{}},
		{name: "nil"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if testCase.needsIPv6 && !IsIPv6("::1") {
				t.Skip("IPv6 is unavailable")
			}
			original := slices.Clone(testCase.addresses)
			selected, found, selectionErr := SelectServiceAddress(testCase.addresses, testCase.required, testCase.preferred)
			require.NoError(t, selectionErr)
			require.Equal(t, testCase.found, found)
			require.Equal(t, testCase.expected, selected)
			require.Equal(t, original, testCase.addresses)
			reversed := slices.Clone(testCase.addresses)
			slices.Reverse(reversed)
			reversedSelection, reversedFound, reversedErr := SelectServiceAddress(reversed, testCase.required, testCase.preferred)
			require.NoError(t, reversedErr)
			require.Equal(t, selected, reversedSelection)
			require.Equal(t, found, reversedFound)
		})
	}
}

// Verifies that SelectServiceAddress resolves arbitrary hostnames as local interface addresses,
// without requiring DNS registration, while preserving the original hostname and port without a selector.
func TestSelectServiceAddressResolvesLocalHostname(t *testing.T) {
	t.Parallel()
	address := apiv2.ServiceAddress{Address: "not-registered-in-dns.invalid", Port: 5002}
	addresses := []apiv2.ServiceAddress{address}
	selected, found, selectionErr := SelectServiceAddress(addresses, IpVersionPreference4, IpVersionPreference6)
	require.NoError(t, selectionErr)
	require.True(t, found)
	require.True(t, IsIPv4(selected.Address))
	require.Equal(t, address.Port, selected.Port)
	localIPs, lookupErr := LookupIP(address.Address)
	require.NoError(t, lookupErr)
	require.True(t, slices.ContainsFunc(localIPs, func(ip net.IP) bool { return IpToString(ip) == selected.Address }))

	unqualified, unqualifiedFound, unqualifiedErr := SelectServiceAddress(addresses, IpVersionPreferenceNone, IpVersionPreference6)
	require.NoError(t, unqualifiedErr)
	require.True(t, unqualifiedFound)
	require.Equal(t, address, unqualified)
	require.Equal(t, []apiv2.ServiceAddress{address}, addresses)
}

// Verifies that SelectServiceAddress reports invalid family policies explicitly instead of treating them as defaults.
func TestSelectServiceAddressRejectsInvalidFamilies(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		required  IpVersionPreference
		preferred IpVersionPreference
	}{
		{name: "invalid required", required: "unknown"},
		{name: "invalid preferred", preferred: "unknown"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			selected, found, selectionErr := SelectServiceAddress(nil, testCase.required, testCase.preferred)
			require.Error(t, selectionErr)
			require.False(t, found)
			require.Zero(t, selected)
		})
	}
}
