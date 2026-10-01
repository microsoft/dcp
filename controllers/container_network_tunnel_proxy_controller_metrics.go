/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/microsoft/dcp/internal/telemetry"
)

var (
	tunnelProxyDiscardedStartupResultCounter  metric.Int64Counter
	tunnelProxyProcessStopFailureCounter      metric.Int64Counter
	tunnelProxyDeleteSubmissionFailureCounter metric.Int64Counter
	tunnelProxyCleanupUnconfirmedCounter      metric.Int64Counter
)

func init() {
	meter := telemetry.GetTelemetrySystem().MeterProvider.Meter("container-network-tunnel-proxy-controller")

	tunnelProxyDiscardedStartupResultCounter = telemetry.NewInt64Counter(
		meter,
		"discardedStartupResults",
		"Number of tunnel proxy startup results discarded because their owning resource state changed",
	)
	tunnelProxyProcessStopFailureCounter = telemetry.NewInt64Counter(
		meter,
		"processStopFailures",
		"Number of tunnel proxy server-process stop attempts that did not confirm cleanup",
	)
	tunnelProxyDeleteSubmissionFailureCounter = telemetry.NewInt64Counter(
		meter,
		"deleteSubmissionFailures",
		"Number of tunnel proxy PhysicalContainer delete submissions that failed",
	)
	tunnelProxyCleanupUnconfirmedCounter = telemetry.NewInt64Counter(
		meter,
		"cleanupUnconfirmed",
		"Number of tunnel proxy cleanup attempts that completed without confirmed cleanup or handoff",
	)
}
