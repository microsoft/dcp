// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
)

var (
	proxylessServiceCounter syncint64.UpDownCounter
	proxiedServiceCounter   syncint64.UpDownCounter
	tcpServiceCounter       syncint64.UpDownCounter
	udpServiceCounter       syncint64.UpDownCounter
	proxyRestartCounter     syncint64.Counter
)

func init() {
	ts := telemetry.GetTelemetrySystem()
	var err error
	svcCtrlMeter := ts.MeterProvider.Meter("service-controller")

	proxylessServiceCounter, err = svcCtrlMeter.SyncInt64().UpDownCounter(
		"proxylessServices",
		instrument.WithDescription("Number of services that do not use a proxy"),
		instrument.WithUnit("{service}"),
	)
	if err != nil {
		panic(err)
	}

	proxiedServiceCounter, err = svcCtrlMeter.SyncInt64().UpDownCounter(
		"proxiedServices",
		instrument.WithDescription("Number of services that use a proxy"),
		instrument.WithUnit("{service}"),
	)
	if err != nil {
		panic(err)
	}

	tcpServiceCounter, err = svcCtrlMeter.SyncInt64().UpDownCounter(
		"tcpServices",
		instrument.WithDescription("Number of services that use TCP"),
		instrument.WithUnit("{service}"),
	)
	if err != nil {
		panic(err)
	}

	udpServiceCounter, err = svcCtrlMeter.SyncInt64().UpDownCounter(
		"udpServices",
		instrument.WithDescription("Number of services that use UDP"),
		instrument.WithUnit("{service}"),
	)
	if err != nil {
		panic(err)
	}

	proxyRestartCounter, err = svcCtrlMeter.SyncInt64().Counter(
		"proxyRestarts",
		instrument.WithDescription("Number of times a proxy has been restarted"),
		instrument.WithUnit("{service}"),
	)
	if err != nil {
		panic(err)
	}
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
