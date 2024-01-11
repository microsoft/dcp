// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"net"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
)

var (
	localhostAllocationModeCounter    syncint64.UpDownCounter
	ipv4ZeroOneAllocationModeCounter  syncint64.UpDownCounter
	ipv4LoopbackAllocationModeCounter syncint64.UpDownCounter
	ipv6ZeroOneAllocationModeCounter  syncint64.UpDownCounter
	proxylessAllocationModeCounter    syncint64.UpDownCounter

	ipv4OnlyCounter  syncint64.UpDownCounter
	ipv6OnlyCounter  syncint64.UpDownCounter
	dualStackCounter syncint64.UpDownCounter

	tcpServiceCounter syncint64.UpDownCounter
	udpServiceCounter syncint64.UpDownCounter
)

func init() {
	ts := telemetry.GetTelemetrySystem()
	svcCtrlMeter := ts.MeterProvider.Meter("service-controller")

	localhostAllocationModeCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "localhostAllocationModeServices", "Number of services that use the localhost address allocation mode")
	ipv4ZeroOneAllocationModeCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "ipv4ZeroOneAllocationModeServices", "Number of services that use the IPv4 127.0.0.1 address allocation mode")
	ipv4LoopbackAllocationModeCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "ipv4LoopbackAllocationModeServices", "Number of services that use the IPv4 random loopback address allocation mode")
	ipv6ZeroOneAllocationModeCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "ipv6ZeroOneAllocationModeServices", "Number of services that use the IPv6 [::1] address allocation mode")
	proxylessAllocationModeCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "proxylessAllocationModeServices", "Number of services that use the proxyless address allocation mode")

	ipv4OnlyCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "ipv4OnlyServices", "Number of services that only use IPv4")
	ipv6OnlyCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "ipv6OnlyServices", "Number of services that only use IPv6")
	dualStackCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "dualStackServices", "Number of services that use both IPv4 and IPv6")

	tcpServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "tcpServices", "Number of services that use TCP")
	udpServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "udpServices", "Number of services that use UDP")
}

func serviceCounters(ctx context.Context, service *apiv1.Service, addend int64) {
	switch service.Spec.AddressAllocationMode {
	case apiv1.AddressAllocationModeLocalhost:
		localhostAllocationModeCounter.Add(ctx, addend)
		stackCounters(ctx, service, addend)
	case apiv1.AddressAllocationModeIPv4ZeroOne:
		ipv4ZeroOneAllocationModeCounter.Add(ctx, addend)
		ipv4OnlyCounter.Add(ctx, addend)
	case apiv1.AddressAllocationModeIPv4Loopback:
		ipv4LoopbackAllocationModeCounter.Add(ctx, addend)
		ipv4OnlyCounter.Add(ctx, addend)
	case apiv1.AddressAllocationModeIPv6ZeroOne:
		ipv6ZeroOneAllocationModeCounter.Add(ctx, addend)
		ipv6OnlyCounter.Add(ctx, addend)
	case apiv1.AddressAllocationModeProxyless:
		proxylessAllocationModeCounter.Add(ctx, addend)
	}

	switch service.Spec.Protocol {
	case apiv1.TCP:
		tcpServiceCounter.Add(ctx, addend)
	case apiv1.UDP:
		udpServiceCounter.Add(ctx, addend)
	}
}

func stackCounters(ctx context.Context, service *apiv1.Service, addend int64) {
	// Localhost can be both IPv4 and IPv6, check to see if the machine supports IPv6
	supportsIPv4 := false
	supportsIPv6 := false
	ips, err := net.LookupIP("localhost")
	if err != nil {
		return // Best effort
	}

	for _, ip := range ips {
		supportsIPv4 = supportsIPv4 || networking.IsIPv4(ip.String())
		supportsIPv6 = supportsIPv6 || networking.IsIPv6(ip.String())
	}

	if supportsIPv4 && supportsIPv6 {
		dualStackCounter.Add(ctx, addend)
	} else if supportsIPv4 {
		ipv4OnlyCounter.Add(ctx, addend)
	} else if supportsIPv6 {
		ipv6OnlyCounter.Add(ctx, addend)
	}
}
