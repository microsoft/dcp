/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/


package controllers

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	"golang.org/x/net/nettest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/telemetry"
)

var (
	localhostAllocationModeCounter    metric.Int64UpDownCounter
	ipv4ZeroOneAllocationModeCounter  metric.Int64UpDownCounter
	ipv4LoopbackAllocationModeCounter metric.Int64UpDownCounter
	ipv6ZeroOneAllocationModeCounter  metric.Int64UpDownCounter
	proxylessAllocationModeCounter    metric.Int64UpDownCounter

	ipv4OnlyCounter  metric.Int64UpDownCounter
	ipv6OnlyCounter  metric.Int64UpDownCounter
	dualStackCounter metric.Int64UpDownCounter

	tcpServiceCounter metric.Int64UpDownCounter
	udpServiceCounter metric.Int64UpDownCounter
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
		stackCounters(ctx, addend)
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

func stackCounters(ctx context.Context, addend int64) {
	// Localhost can be both IPv4 and IPv6, check to see if the machine supports IPv6
	supportsIPv4 := nettest.SupportsIPv4()
	supportsIPv6 := nettest.SupportsIPv6()

	if supportsIPv4 && supportsIPv6 {
		dualStackCounter.Add(ctx, addend)
	} else if supportsIPv4 {
		ipv4OnlyCounter.Add(ctx, addend)
	} else if supportsIPv6 {
		ipv6OnlyCounter.Add(ctx, addend)
	}
}
