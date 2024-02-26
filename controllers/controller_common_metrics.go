// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/microsoft/usvc-apiserver/internal/telemetry"
)

var (
	metadataOrSpecSaveCounter metric.Int64Counter
	statusSaveCounter         metric.Int64Counter
	saveFailedCounter         metric.Int64Counter

	getSucceededCounter metric.Int64Counter
	getFailedCounter    metric.Int64Counter
	getNotFoundCounter  metric.Int64Counter
)

func init() {
	ts := telemetry.GetTelemetrySystem()
	svcCtrlMeter := ts.MeterProvider.Meter("controller-common")

	metadataOrSpecSaveCounter = telemetry.NewInt64Counter(svcCtrlMeter, "metadataSave", "Number of times metadata has been saved")
	statusSaveCounter = telemetry.NewInt64Counter(svcCtrlMeter, "statusSave", "Number of times status has been saved")
	saveFailedCounter = telemetry.NewInt64Counter(svcCtrlMeter, "saveFailed", "Number of times save has failed")

	getSucceededCounter = telemetry.NewInt64Counter(svcCtrlMeter, "getSucceeded", "Number of times get has succeeded")
	getFailedCounter = telemetry.NewInt64Counter(svcCtrlMeter, "getFailed", "Number of times get has failed, excluding NotFound")
	getNotFoundCounter = telemetry.NewInt64Counter(svcCtrlMeter, "getNotFound", "Number of times get has returned NotFound")
}
