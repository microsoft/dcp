/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that logical ports require unique names and exactly one valid same-namespace
// service reference or named service template, including on existing-mode workloads.
func TestLogicalPortsValidateServiceBindings(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		bindings []LogicalServiceBinding
		valid    bool
	}{
		{"reference", []LogicalServiceBinding{{Name: "http", ServiceRef: "service"}}, true},
		{"qualified reference", []LogicalServiceBinding{{Name: "http", ServiceRef: "test-namespace/service"}}, true},
		{"template", []LogicalServiceBinding{{Name: "http", Service: &LogicalServiceTemplate{Name: "management"}}}, true},
		{"multiple", []LogicalServiceBinding{{Name: "http", ServiceRef: "service"}, {Name: "metrics", Service: &LogicalServiceTemplate{Name: "metrics"}}}, true},
		{"missing service", []LogicalServiceBinding{{Name: "http"}}, false},
		{"both sources", []LogicalServiceBinding{{Name: "http", ServiceRef: "service", Service: &LogicalServiceTemplate{Name: "management"}}}, false},
		{"cross namespace", []LogicalServiceBinding{{Name: "http", ServiceRef: "other/service"}}, false},
		{"invalid reference", []LogicalServiceBinding{{Name: "http", ServiceRef: "INVALID"}}, false},
		{"missing port name", []LogicalServiceBinding{{ServiceRef: "service"}}, false},
		{"invalid port name", []LogicalServiceBinding{{Name: "HTTP", ServiceRef: "service"}}, false},
		{"duplicate port name", []LogicalServiceBinding{{Name: "http", ServiceRef: "first"}, {Name: "http", ServiceRef: "second"}}, false},
		{"missing template name", []LogicalServiceBinding{{Name: "http", Service: &LogicalServiceTemplate{}}}, false},
		{"duplicate template name", []LogicalServiceBinding{{Name: "http", Service: &LogicalServiceTemplate{Name: "shared"}}, {Name: "metrics", Service: &LogicalServiceTemplate{Name: "shared"}}}, false},
		{"invalid template protocol", []LogicalServiceBinding{{Name: "http", Service: &LogicalServiceTemplate{Name: "http", LogicalServiceSpec: LogicalServiceSpec{Protocol: "HTTP"}}}}, false},
		{"invalid template port", []LogicalServiceBinding{{Name: "http", Service: &LogicalServiceTemplate{Name: "http", LogicalServiceSpec: LogicalServiceSpec{Port: -1}}}}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			containerPorts := make([]LogicalContainerPort, len(testCase.bindings))
			processPorts := make([]LogicalProcessPort, len(testCase.bindings))
			for portIndex, binding := range testCase.bindings {
				containerPorts[portIndex] = LogicalContainerPort{LogicalServiceBinding: binding, ContainerPort: int32(8080 + portIndex)}
				processPorts[portIndex] = LogicalProcessPort{LogicalServiceBinding: binding, Port: int32(8080 + portIndex)}
			}
			container := &LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
				Spec: LogicalContainerSpec{Mode: LogicalContainerModeExisting, ContainerName: "runtime", Ports: containerPorts}}
			pid := int64(42)
			resource := &LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"},
				Spec: LogicalProcessSpec{Mode: LogicalProcessModeExisting, PID: &pid, IdentityTimestamp: metav1.NowMicro(), Ports: processPorts}}
			require.Equal(t, testCase.valid, len(container.Validate(context.Background())) == 0, "%v", container.Validate(context.Background()))
			require.Equal(t, testCase.valid, len(resource.Validate(context.Background())) == 0, "%v", resource.Validate(context.Background()))
		})
	}
}

// Verifies that logical ports enforce runtime port bounds and generated service-name length,
// while process ports allow dynamic allocation and physical container ports retain ranges.
func TestLogicalPortsValidatePortConfiguration(t *testing.T) {
	t.Parallel()
	binding := LogicalServiceBinding{Name: "http", ServiceRef: "service"}
	container := &LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
		Spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"}, Ports: []LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}}}}
	for _, invalidPort := range []int32{-1, 0, 65536} {
		changed := container.DeepCopy()
		changed.Spec.Ports[0].ContainerPort = invalidPort
		require.NotEmpty(t, changed.Validate(context.Background()))
	}
	for _, invalidHostPort := range []int32{-1, 65536} {
		changed := container.DeepCopy()
		changed.Spec.Ports[0].HostPort = invalidHostPort
		require.NotEmpty(t, changed.Validate(context.Background()))
	}
	longName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	container.Name = longName
	container.Spec.Ports[0].ServiceRef = ""
	container.Spec.Ports[0].Service = &LogicalServiceTemplate{Name: "management"}
	require.Contains(t, container.Validate(context.Background()).ToAggregate().Error(), "generated service name")
	resource := &LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"},
		Spec: LogicalProcessSpec{Process: &LogicalProcessConfig{ExecutablePath: "app"}, Ports: []LogicalProcessPort{{LogicalServiceBinding: binding}}}}
	require.Empty(t, resource.Validate(context.Background()))
	resource.Spec.Ports[0].Port = 65536
	require.NotEmpty(t, resource.Validate(context.Background()))
	require.Empty(t, ValidateContainerPorts([]ContainerPort{{ContainerPort: 8080, RangeSize: 2}}, nil))
}

// Verifies that unified logical ports serialize at spec.ports with named service templates,
// without runtime protocol duplication or a ports collection in the creation configuration.
func TestLogicalPortsJSONAndDeepCopy(t *testing.T) {
	t.Parallel()
	container := &LogicalContainer{Spec: LogicalContainerSpec{
		Container: &LogicalContainerConfig{ImageRef: "image"},
		Ports: []LogicalContainerPort{{ContainerPort: 8080, LogicalServiceBinding: LogicalServiceBinding{
			Name: "http", Service: &LogicalServiceTemplate{Name: "management", LogicalServiceSpec: LogicalServiceSpec{Protocol: commonapi.PortProtocolUDP}},
		}}},
	}}
	encoded, encodeErr := json.Marshal(container)
	require.NoError(t, encodeErr)
	require.Contains(t, string(encoded), `"ports":[{"name":"http","service":{"protocol":"UDP","name":"management"},"containerPort":8080}]`)
	require.NotContains(t, string(encoded), `"rangeSize"`)
	require.NotContains(t, string(encoded), `"container":{"imageRef":"image","ports"`)
	copy := container.DeepCopy()
	copy.Spec.Ports[0].Service.Name = "changed"
	require.Equal(t, "management", container.Spec.Ports[0].Service.Name)
	physical := container.Spec.Ports[0].ToContainerPort(commonapi.PortProtocolUDP)
	require.Equal(t, int32(8080), physical.ContainerPort)
	require.Equal(t, commonapi.PortProtocolUDP, physical.Protocol)
	require.Equal(t, int32(1), physical.EffectiveRangeSize())
}

// Verifies that LogicalProcessSpec.GetLifecycleKey excludes service registrations and listening-port
// declarations while still changing when the evaluated process arguments or environment change.
func TestLogicalProcessLifecycleKeyIgnoresServiceDeclarations(t *testing.T) {
	t.Parallel()
	spec := &LogicalProcessSpec{Process: &LogicalProcessConfig{ExecutablePath: "app"},
		Ports: []LogicalProcessPort{{LogicalServiceBinding: LogicalServiceBinding{Name: "http", ServiceRef: "service"}, Port: 8080}}}
	original := spec.DeepCopy()
	key, _, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	for _, testCase := range []struct {
		name   string
		change func(*LogicalProcessSpec)
	}{
		{"port name", func(changed *LogicalProcessSpec) { changed.Ports[0].Name = "renamed" }},
		{"service reference", func(changed *LogicalProcessSpec) { changed.Ports[0].ServiceRef = "other-service" }},
		{"listening address", func(changed *LogicalProcessSpec) { changed.Ports[0].Address = "[::1]" }},
		{"listening port", func(changed *LogicalProcessSpec) { changed.Ports[0].Port++ }},
		{"inline template", func(changed *LogicalProcessSpec) {
			changed.Ports[0].ServiceRef = ""
			changed.Ports[0].Service = &LogicalServiceTemplate{Name: "inline", LogicalServiceSpec: LogicalServiceSpec{Address: "[::1]", Port: 6000}}
		}},
		{"no ports", func(changed *LogicalProcessSpec) { changed.Ports = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := spec.DeepCopy()
			testCase.change(changed)
			changedKey, _, changedErr := changed.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.Equal(t, key, changedKey)
		})
	}
	for _, testCase := range []struct {
		name   string
		change func(*LogicalProcessSpec)
	}{
		{"evaluated argument", func(changed *LogicalProcessSpec) { changed.Process.Args = []string{"--listen=9090"} }},
		{"evaluated environment", func(changed *LogicalProcessSpec) {
			changed.Process.Env = []commonapi.EnvVar{{Name: "PORT", Value: "9090"}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := spec.DeepCopy()
			testCase.change(changed)
			changedKey, _, changedErr := changed.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.NotEqual(t, key, changedKey)
		})
	}
	require.Equal(t, original, spec)
}

// Verifies that LogicalContainerSpec.GetLifecycleKey ignores service identity and proxy configuration,
// but includes the resolved runtime port fields that V1 used for container reuse.
func TestLogicalContainerLifecycleKeyUsesRuntimePorts(t *testing.T) {
	t.Parallel()
	spec := &LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"},
		Ports: []LogicalContainerPort{{LogicalServiceBinding: LogicalServiceBinding{
			Name: "http", Service: &LogicalServiceTemplate{Name: "management"},
		}, ContainerPort: 8080}}}
	runtimePorts := []ContainerPort{{ContainerPort: 8080, HostIP: "127.0.0.1", Protocol: commonapi.PortProtocolTCP}}
	original, originalRuntimePorts := spec.DeepCopy(), append([]ContainerPort(nil), runtimePorts...)
	key, _, keyErr := spec.GetLifecycleKey(runtimePorts)
	require.NoError(t, keyErr)
	for _, testCase := range []struct {
		name   string
		change func(*LogicalContainerSpec)
	}{
		{"logical port name", func(changed *LogicalContainerSpec) { changed.Ports[0].Name = "renamed" }},
		{"template name", func(changed *LogicalContainerSpec) { changed.Ports[0].Service.Name = "renamed" }},
		{"service reference", func(changed *LogicalContainerSpec) {
			changed.Ports[0].Service = nil
			changed.Ports[0].ServiceRef = "other-service"
		}},
		{"proxy address", func(changed *LogicalContainerSpec) { changed.Ports[0].Service.Address = "[::1]" }},
		{"proxy port", func(changed *LogicalContainerSpec) { changed.Ports[0].Service.Port = 6000 }},
		{"proxy allocation mode", func(changed *LogicalContainerSpec) {
			changed.Ports[0].Service.AddressAllocationMode = ServiceAddressAllocationModeProxyless
		}},
		{"serving address", func(changed *LogicalContainerSpec) { changed.Ports[0].Address = "0.0.0.0" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := spec.DeepCopy()
			testCase.change(changed)
			changedKey, _, changedErr := changed.GetLifecycleKey(runtimePorts)
			require.NoError(t, changedErr)
			require.Equal(t, key, changedKey)
		})
	}
	for _, testCase := range []struct {
		name   string
		change func(*ContainerPort)
	}{
		{"container port", func(changed *ContainerPort) { changed.ContainerPort++ }},
		{"host port", func(changed *ContainerPort) { changed.HostPort = 9000 }},
		{"host address", func(changed *ContainerPort) { changed.HostIP = "::1" }},
		{"protocol", func(changed *ContainerPort) { changed.Protocol = commonapi.PortProtocolUDP }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changedPorts := append([]ContainerPort(nil), runtimePorts...)
			testCase.change(&changedPorts[0])
			changedKey, _, changedErr := spec.GetLifecycleKey(changedPorts)
			require.NoError(t, changedErr)
			require.NotEqual(t, key, changedKey)
		})
	}
	defaultedPorts := append([]ContainerPort(nil), runtimePorts...)
	defaultedPorts[0].Protocol, defaultedPorts[0].RangeSize = "", 1
	defaultedKey, _, defaultedErr := spec.GetLifecycleKey(defaultedPorts)
	require.NoError(t, defaultedErr)
	require.Equal(t, key, defaultedKey)
	require.Equal(t, original, spec)
	require.Equal(t, originalRuntimePorts, runtimePorts)

	_, _, missingPortsErr := spec.GetLifecycleKey(nil)
	require.Error(t, missingPortsErr)
	spec.LifecycleKey = "explicit"
	explicitKey, computed, explicitErr := spec.GetLifecycleKey(nil)
	require.NoError(t, explicitErr)
	require.False(t, computed)
	require.Equal(t, "explicit", explicitKey)
}

// Verifies that LogicalProcess.Validate requires fixed serving ports in persistent and existing modes
// regardless of lifecycle key, while preserving dynamic allocation for session processes.
func TestLogicalProcessPortValidationByMode(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		mode  LogicalProcessMode
		port  int32
		valid bool
	}{
		{"default session dynamic", "", 0, true},
		{"session dynamic", LogicalProcessModeSession, 0, true},
		{"persistent dynamic rejected", LogicalProcessModePersistent, 0, false},
		{"persistent minimum", LogicalProcessModePersistent, 1, true},
		{"persistent explicit", LogicalProcessModePersistent, 8080, true},
		{"persistent maximum", LogicalProcessModePersistent, 65535, true},
		{"existing dynamic rejected", LogicalProcessModeExisting, 0, false},
		{"existing explicit", LogicalProcessModeExisting, 8080, true},
		{"existing maximum", LogicalProcessModeExisting, 65535, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			for _, lifecycleKey := range []string{"", "explicit"} {
				spec := LogicalProcessSpec{Mode: testCase.mode, LifecycleKey: lifecycleKey, Ports: []LogicalProcessPort{{
					LogicalServiceBinding: LogicalServiceBinding{Name: "http", ServiceRef: "service"}, Port: testCase.port,
				}}}
				if testCase.mode == LogicalProcessModeExisting {
					pid := int64(42)
					spec.PID, spec.IdentityTimestamp = &pid, metav1.NowMicro()
				} else {
					spec.Process = &LogicalProcessConfig{ExecutablePath: "app"}
				}
				resource := &LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}, Spec: spec}
				validationErrors := resource.Validate(context.Background())
				if testCase.valid {
					require.Empty(t, validationErrors, "lifecycle key %q", lifecycleKey)
				} else {
					require.Len(t, validationErrors, 1, "lifecycle key %q", lifecycleKey)
					require.Equal(t, "spec.ports[0].port", validationErrors[0].Field)
					require.Contains(t, validationErrors[0].Detail, "an explicit serving port is required")
				}
			}
		})
	}
}
