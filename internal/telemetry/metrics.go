// Copyright (c) Microsoft Corporation. All rights reserved.

package telemetry

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
	"go.opentelemetry.io/otel/metric/unit"
)

func NewInt64Counter(meter metric.Meter, name string, description string) syncint64.Counter {
	counter, err := meter.SyncInt64().Counter(
		name,
		instrument.WithDescription(description),
		instrument.WithUnit(unit.Dimensionless),
	)
	if err != nil {
		panic(err)
	}
	return counter
}

func NewInt64UpDownCounter(meter metric.Meter, name string, description string) syncint64.UpDownCounter {
	counter, err := meter.SyncInt64().UpDownCounter(
		name,
		instrument.WithDescription(description),
		instrument.WithUnit(unit.Dimensionless),
	)
	if err != nil {
		panic(err)
	}
	return counter
}
