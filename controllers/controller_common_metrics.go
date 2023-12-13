// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"go.opentelemetry.io/otel/metric/instrument/syncint64"

	"github.com/microsoft/usvc-apiserver/internal/telemetry"
)

var (
	metadataOrSpecSaveCounter syncint64.Counter
	statusSaveCounter         syncint64.Counter
	saveFailedCounter         syncint64.Counter
)

func init() {
	ts := telemetry.GetTelemetrySystem()
	svcCtrlMeter := ts.MeterProvider.Meter("controller-common")

	metadataOrSpecSaveCounter = telemetry.NewInt64Counter(svcCtrlMeter, "metadataSave", "Number of times metadata has been saved")
	statusSaveCounter = telemetry.NewInt64Counter(svcCtrlMeter, "statusSave", "Number of times status has been saved")
	saveFailedCounter = telemetry.NewInt64Counter(svcCtrlMeter, "saveFailed", "Number of times save has failed")
}
