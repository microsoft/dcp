// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"testing"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/stretchr/testify/require"
)

func TestGetFreePortTCP(t *testing.T) {
	testPortsNotEqual(t, apiv1.TCP)
}

func TestGetFreePortUDP(t *testing.T) {
	testPortsNotEqual(t, apiv1.UDP)
}

func testPortsNotEqual(t *testing.T, protocol apiv1.PortProtocol) {
	port1, err1 := GetFreePort(protocol, "localhost")
	port2, err2 := GetFreePort(protocol, "localhost")

	require.NoError(t, err1, "error1: %v", err1)
	require.NoError(t, err2, "error2: %v", err2)

	require.NotEqual(t, port1, port2, "GetFreePort must not return the same port when called twice in immediate succession")
}
