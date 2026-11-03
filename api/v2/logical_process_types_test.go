/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that LogicalProcess validates mode-specific configuration and identity requirements.
func TestLogicalProcessValidate(t *testing.T) {
	pid := int64(123)
	badPID := int64(math.MaxUint32) + 1
	timestamp := metav1.NewMicroTime(time.Now())
	config := &LogicalProcessConfig{ExecutablePath: "program"}
	for _, testCase := range []struct {
		name  string
		spec  LogicalProcessSpec
		valid bool
	}{
		{"session", LogicalProcessSpec{Process: config}, true},
		{"persistent", LogicalProcessSpec{Mode: LogicalProcessModePersistent, Process: config, MonitorPID: &pid, MonitorTimestamp: timestamp}, true},
		{"existing", LogicalProcessSpec{Mode: LogicalProcessModeExisting, PID: &pid, IdentityTimestamp: timestamp}, true},
		{"missing process", LogicalProcessSpec{}, false},
		{"blank executable", LogicalProcessSpec{Process: &LogicalProcessConfig{ExecutablePath: " "}}, false},
		{"session pid", LogicalProcessSpec{Process: config, PID: &pid}, false},
		{"session identity", LogicalProcessSpec{Process: config, IdentityTimestamp: timestamp}, false},
		{"session monitor", LogicalProcessSpec{Process: config, MonitorPID: &pid, MonitorTimestamp: timestamp}, false},
		{"incomplete monitor", LogicalProcessSpec{Mode: LogicalProcessModePersistent, Process: config, MonitorPID: &pid}, false},
		{"existing pid only", LogicalProcessSpec{Mode: LogicalProcessModeExisting, PID: &pid}, false},
		{"existing timestamp only", LogicalProcessSpec{Mode: LogicalProcessModeExisting, IdentityTimestamp: timestamp}, false},
		{"existing configuration", LogicalProcessSpec{Mode: LogicalProcessModeExisting, PID: &pid, IdentityTimestamp: timestamp, Process: config}, false},
		{"invalid pid", LogicalProcessSpec{Mode: LogicalProcessModeExisting, PID: &badPID, IdentityTimestamp: timestamp}, false},
		{"cleanup unsupported", LogicalProcessSpec{Mode: "cleanup", Process: config}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resource := &LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test"}, Spec: testCase.spec}
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0)
		})
	}
}

// Verifies that LogicalProcessSpec lifecycle keys normalize defaults and environment ordering,
// preserve duplicate precedence and argument order, and include the persistent monitor identity.
func TestLogicalProcessLifecycleKey(t *testing.T) {
	t.Parallel()
	spec := LogicalProcessSpec{Process: &LogicalProcessConfig{
		ExecutablePath: "program", Args: []string{"a", "b"}, Env: []commonapi.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}},
	}}
	key, computed, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.True(t, computed)
	require.NotEmpty(t, key)
	reordered := spec.DeepCopy()
	reordered.Process.Env[0], reordered.Process.Env[1] = reordered.Process.Env[1], reordered.Process.Env[0]
	inherit := true
	reordered.Process.InheritEnvironment = &inherit
	sameKey, _, sameErr := reordered.GetLifecycleKey()
	require.NoError(t, sameErr)
	require.Equal(t, key, sameKey)
	require.Equal(t, "B", spec.Process.Env[0].Name, "hashing must not mutate the desired configuration")
	for _, mutate := range []func(*LogicalProcessSpec){
		func(changed *LogicalProcessSpec) { changed.Process.ExecutablePath = "other" },
		func(changed *LogicalProcessSpec) { changed.Process.WorkingDirectory = "/other" },
		func(changed *LogicalProcessSpec) {
			changed.Process.Args[0], changed.Process.Args[1] = changed.Process.Args[1], changed.Process.Args[0]
		},
		func(changed *LogicalProcessSpec) { noInherit := false; changed.Process.InheritEnvironment = &noInherit },
		func(changed *LogicalProcessSpec) { changed.Process.Env[0].Value = "3" },
		func(changed *LogicalProcessSpec) {
			pid := int64(1)
			changed.MonitorPID = &pid
			changed.MonitorTimestamp = metav1.NewMicroTime(time.Now())
		},
	} {
		changed := spec.DeepCopy()
		mutate(changed)
		changedKey, _, changedErr := changed.GetLifecycleKey()
		require.NoError(t, changedErr)
		require.NotEqual(t, key, changedKey)
	}
	duplicates := spec.DeepCopy()
	duplicates.Process.Env = []commonapi.EnvVar{{Name: "A", Value: "first"}, {Name: "A", Value: "last"}}
	firstKey, _, firstErr := duplicates.GetLifecycleKey()
	require.NoError(t, firstErr)
	duplicates.Process.Env[0], duplicates.Process.Env[1] = duplicates.Process.Env[1], duplicates.Process.Env[0]
	lastKey, _, lastErr := duplicates.GetLifecycleKey()
	require.NoError(t, lastErr)
	require.NotEqual(t, firstKey, lastKey)
	effective := duplicates.DeepCopy()
	effective.Process.Env = []commonapi.EnvVar{duplicates.Process.Env[1]}
	effectiveKey, _, effectiveErr := effective.GetLifecycleKey()
	require.NoError(t, effectiveErr)
	require.Equal(t, lastKey, effectiveKey, "overwritten environment declarations must not affect the key")
	explicitKey, explicitComputed, explicitErr := (&LogicalProcessSpec{LifecycleKey: "explicit"}).GetLifecycleKey()
	require.NoError(t, explicitErr)
	require.False(t, explicitComputed)
	require.Equal(t, "explicit", explicitKey)
	_, _, missingErr := (&LogicalProcessSpec{}).GetLifecycleKey()
	require.Error(t, missingErr)
}

// Verifies that LogicalProcessSpec hashes executable and argument boundaries and monitor identity,
// while excluding lifecycle policy and treating nil and empty collections equivalently.
func TestLogicalProcessLifecycleKeyInputs(t *testing.T) {
	t.Parallel()
	pid := int64(123)
	spec := LogicalProcessSpec{Mode: LogicalProcessModePersistent, MonitorPID: &pid,
		MonitorTimestamp: metav1.NewMicroTime(time.Unix(100, 0)),
		Process:          &LogicalProcessConfig{ExecutablePath: "ab", WorkingDirectory: "c", Args: []string{"a", "bc"}}}
	original := spec.DeepCopy()
	key, _, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.Equal(t, original, &spec)
	for _, testCase := range []struct {
		name   string
		change func(*LogicalProcessSpec)
	}{
		{"path boundaries", func(changed *LogicalProcessSpec) {
			changed.Process.ExecutablePath, changed.Process.WorkingDirectory = "a", "bc"
		}},
		{"argument boundaries", func(changed *LogicalProcessSpec) { changed.Process.Args = []string{"ab", "c"} }},
		{"monitor pid", func(changed *LogicalProcessSpec) { *changed.MonitorPID = 124 }},
		{"monitor timestamp", func(changed *LogicalProcessSpec) {
			changed.MonitorTimestamp = metav1.NewMicroTime(changed.MonitorTimestamp.Add(time.Microsecond))
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			changed := spec.DeepCopy()
			testCase.change(changed)
			changedKey, _, changedErr := changed.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.NotEqual(t, key, changedKey)
		})
	}
	policy := spec.DeepCopy()
	policy.Mode, policy.Stop = LogicalProcessModeSession, true
	policy.MonitorPID, policy.MonitorTimestamp = nil, metav1.MicroTime{}
	baseline := spec.DeepCopy()
	baseline.MonitorPID, baseline.MonitorTimestamp = nil, metav1.MicroTime{}
	baselineKey, _, baselineErr := baseline.GetLifecycleKey()
	require.NoError(t, baselineErr)
	policyKey, _, policyErr := policy.GetLifecycleKey()
	require.NoError(t, policyErr)
	require.Equal(t, baselineKey, policyKey)
	empty := LogicalProcessSpec{Process: &LogicalProcessConfig{ExecutablePath: "program"}}
	nilKey, _, nilErr := empty.GetLifecycleKey()
	require.NoError(t, nilErr)
	empty.Process.Args, empty.Process.Env = []string{}, []commonapi.EnvVar{}
	emptyKey, _, emptyErr := empty.GetLifecycleKey()
	require.NoError(t, emptyErr)
	require.Equal(t, nilKey, emptyKey)
}

// Verifies that LogicalProcess permits only monotonic stop changes and uses a namespace-scoped persistent identity.
func TestLogicalProcessUpdateAndLeaseKey(t *testing.T) {
	resource := &LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test"},
		Spec: LogicalProcessSpec{Process: &LogicalProcessConfig{ExecutablePath: "program"}}}
	require.Equal(t, LogicalProcessModeSession, resource.Spec.EffectiveMode())
	require.Equal(t, "processes/test/process", resource.GetLeaseKey())
	resource.Namespace, resource.Name = "containers", "app"
	container := &LogicalContainer{Spec: LogicalContainerSpec{ContainerName: "app"}}
	require.NotEqual(t, resource.GetLeaseKey(), container.GetLeaseKey())
	resource.Namespace = "containernetworks"
	network := &LogicalContainerNetwork{Spec: LogicalContainerNetworkSpec{NetworkName: "app"}}
	require.NotEqual(t, resource.GetLeaseKey(), network.GetLeaseKey())
	stopped := resource.DeepCopy()
	stopped.Spec.Stop = true
	require.Empty(t, stopped.ValidateUpdate(context.Background(), resource))
	require.NotEmpty(t, resource.ValidateUpdate(context.Background(), stopped))
	changed := resource.DeepCopy()
	changed.Spec.Process.Args = []string{"different"}
	require.NotEmpty(t, changed.ValidateUpdate(context.Background(), resource))
}

// Verifies that PhysicalProcess allows an optional expected identity only with a PID reference and freezes it on update.
func TestPhysicalProcessExpectedIdentity(t *testing.T) {
	pid := int64(123)
	resource := &PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test"},
		Spec: PhysicalProcessSpec{PID: &pid, IdentityTimestamp: metav1.NewMicroTime(time.Now())}}
	require.Empty(t, resource.Validate(context.Background()))
	changed := resource.DeepCopy()
	changed.Spec.IdentityTimestamp = metav1.MicroTime{}
	require.NotEmpty(t, changed.ValidateUpdate(context.Background(), resource))
	resource.Spec.PID = nil
	resource.Spec.Process = &PhysicalProcessConfig{ExecutablePath: "program"}
	require.NotEmpty(t, resource.Validate(context.Background()))
}
