/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package health

import (
	"context"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
)

// HealthProbeDescriptor contains data necessary to run and report results of a health probe.
type healthProbeDescriptor struct {
	// Probe definition
	probe *apiv1.HealthProbe

	// The owner of the probe (e.g. Container, Executable, etc.)
	owner commonapi.DcpModelObject

	// The owner NamespacedNameWithKind (cached for quick access)
	ownerNNK commonapi.NamespacedNameWithKind

	// The serialized owner NamespacedNameWithKind (cached for quick access)
	ownerDescription string

	// The probe identifier (cached for quick access)
	identifier healthProbeIdentifier

	// The cancellation function for the probe execution context.
	// The function is nil if the probe is not currently running.
	cancelExecution context.CancelFunc

	// The last result of the probe execution. Nil if the probe has never been executed.
	lastResult *apiv1.HealthProbeResult

	// The next time the probe should be executed.
	nextExecutionTime time.Time
}

func newHealthProbeDescriptor(probe *apiv1.HealthProbe, owner commonapi.DcpModelObject) *healthProbeDescriptor {
	hps := &healthProbeDescriptor{
		probe:           probe,
		owner:           owner,
		ownerNNK:        commonapi.GetNamespacedNameWithKind(owner),
		identifier:      getIdentifier(probe, owner),
		cancelExecution: nil,
		lastResult:      nil,
	}
	hps.ownerDescription = hps.ownerNNK.String()
	hps.computeNextExecutionTime()
	return hps
}

func (hpd *healthProbeDescriptor) computeNextExecutionTime() {
	schedule := hpd.probe.Schedule

	if schedule.Kind == apiv1.HealthProbeScheduleUntilSuccess && hpd.lastResult != nil && hpd.lastResult.Outcome == apiv1.HealthProbeOutcomeSuccess {
		hpd.nextExecutionTime = unknownFuture
		return
	}

	now := time.Now()

	if hpd.lastResult == nil {
		// The probe has never been executed before
		if schedule.InitialDelay != nil {
			hpd.nextExecutionTime = now.Add(schedule.InitialDelay.Duration)
		} else {
			hpd.nextExecutionTime = now
		}
	} else {
		desired := hpd.lastResult.Timestamp.Add(schedule.Interval.Duration)
		if desired.Before(now) {
			// The probe is already late for the next execution, but do not set the time in the past.
			// This prevents the execution of the probe from starving other probe executions.
			hpd.nextExecutionTime = now
		} else {
			hpd.nextExecutionTime = desired
		}
	}
}

// Used by the priority queue to compare two health probes by their next execution time.
func compareExecutionTimes(d1, d2 any) int {
	hpd1 := d1.(*healthProbeDescriptor)
	hpd2 := d2.(*healthProbeDescriptor)

	if hpd1.nextExecutionTime.Before(hpd2.nextExecutionTime) {
		return -1
	}
	if hpd1.nextExecutionTime.After(hpd2.nextExecutionTime) {
		return 1
	}
	return 0
}
