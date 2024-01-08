// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
)

var (
	proxylessServiceCounter syncint64.UpDownCounter
	proxiedServiceCounter   syncint64.UpDownCounter
	tcpServiceCounter       syncint64.UpDownCounter
	udpServiceCounter       syncint64.UpDownCounter
)

func init() {
	ts := telemetry.GetTelemetrySystem()
	svcCtrlMeter := ts.MeterProvider.Meter("service-controller")

	proxylessServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "proxylessServices", "Number of services that do not use a proxy")
	proxiedServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "proxiedServices", "Number of services that use a proxy")
	tcpServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "tcpServices", "Number of services that use TCP")
	udpServiceCounter = telemetry.NewInt64UpDownCounter(svcCtrlMeter, "udpServices", "Number of services that use UDP")
}

func serviceCounters(ctx context.Context, service *apiv1.Service, addend int64) {
	if service.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless {
		proxylessServiceCounter.Add(ctx, addend)
	} else {
		proxiedServiceCounter.Add(ctx, addend)
	}

	if service.Spec.Protocol == apiv1.TCP {
		tcpServiceCounter.Add(ctx, addend)
	} else {
		udpServiceCounter.Add(ctx, addend)
	}
}
