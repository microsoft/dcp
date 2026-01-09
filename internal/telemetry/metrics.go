/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/


package telemetry

import (
	"go.opentelemetry.io/otel/metric"
)

func NewInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	counter, err := meter.Int64Counter(
		name,
		metric.WithDescription(description),
		metric.WithUnit("1"), // dimensionless
	)
	if err != nil {
		panic(err)
	}
	return counter
}

func NewInt64UpDownCounter(meter metric.Meter, name string, description string) metric.Int64UpDownCounter {
	counter, err := meter.Int64UpDownCounter(
		name,
		metric.WithDescription(description),
		metric.WithUnit("1"), // dimensionless
	)
	if err != nil {
		panic(err)
	}
	return counter
}
