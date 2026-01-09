/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package health

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/maps"
)

type probeCall struct {
	Probe     *apiv1.HealthProbe
	Timestamp time.Time
}

type mockProbeExecutor struct {
	ProbeCalls []probeCall
	lock       *sync.Mutex
}

func newMockProbeExecutor() *mockProbeExecutor {
	return &mockProbeExecutor{
		lock: &sync.Mutex{},
	}
}

func (m *mockProbeExecutor) Execute(executionCtx context.Context, probe *apiv1.HealthProbe, _ commonapi.DcpModelObject, probeID healthProbeIdentifier) (apiv1.HealthProbeResult, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	now := time.Now()
	m.ProbeCalls = append(m.ProbeCalls, probeCall{
		Probe:     probe,
		Timestamp: time.Now(),
	})
	return apiv1.HealthProbeResult{
		ProbeName: probe.Name,
		Outcome:   apiv1.HealthProbeOutcomeSuccess,
		Timestamp: metav1.NewMicroTime(now),
	}, nil
}

// Returns the names of all probes that have been called.
func (m *mockProbeExecutor) AllProbesCalled() []string {
	m.lock.Lock()
	defer m.lock.Unlock()
	tmpMap := make(map[string]struct{})
	for _, call := range m.ProbeCalls {
		tmpMap[call.Probe.Name] = struct{}{}
	}
	probeNames := maps.Keys(tmpMap)
	return probeNames
}

var _ HealthProbeExecutor = (*mockProbeExecutor)(nil)
