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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that LogicalContainerNetwork.Validate enforces lifecycle modes, creation configuration,
// runtime name requirements, and label validation.
func TestLogicalContainerNetworkValidate(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		spec           LogicalContainerNetworkSpec
		expectedFields []string
	}{
		{"default session", LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{}}, nil},
		{"explicit session", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModeSession, Network: &LogicalContainerNetworkConfig{}}, nil},
		{"named session", LogicalContainerNetworkSpec{NetworkName: "a", Network: &LogicalContainerNetworkConfig{}}, nil},
		{"persistent", LogicalContainerNetworkSpec{
			Mode: LogicalContainerNetworkModePersistent, NetworkName: "persistent-network",
			Network: &LogicalContainerNetworkConfig{IPv6: true, Labels: []commonapi.Label{{Key: "app", Value: "test"}}},
		}, nil},
		{"existing", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModeExisting, NetworkName: "existing-network"}, nil},
		{"missing default config", LogicalContainerNetworkSpec{}, []string{"spec.network"}},
		{"missing session config", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModeSession}, []string{"spec.network"}},
		{"missing persistent config", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModePersistent, NetworkName: "network"}, []string{"spec.network"}},
		{"missing persistent name", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModePersistent, Network: &LogicalContainerNetworkConfig{}}, []string{"spec.networkName"}},
		{"missing existing name", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModeExisting}, []string{"spec.networkName"}},
		{"existing with config", LogicalContainerNetworkSpec{Mode: LogicalContainerNetworkModeExisting, NetworkName: "network", Network: &LogicalContainerNetworkConfig{}}, []string{"spec.network"}},
		{"cleanup unsupported", LogicalContainerNetworkSpec{Mode: "cleanup"}, []string{"spec.mode"}},
		{"unknown mode", LogicalContainerNetworkSpec{Mode: "unknown"}, []string{"spec.mode"}},
		{"invalid runtime name", LogicalContainerNetworkSpec{NetworkName: "invalid/name", Network: &LogicalContainerNetworkConfig{}}, []string{"spec.networkName"}},
		{"blank runtime name", LogicalContainerNetworkSpec{NetworkName: " ", Network: &LogicalContainerNetworkConfig{}}, []string{"spec.networkName"}},
		{"invalid label", LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{Labels: []commonapi.Label{{Value: "value"}}}}, []string{"spec.network.labels[0].key"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			network := &LogicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test-namespace"},
				Spec:       testCase.spec,
			}
			errorList := network.Validate(context.Background())
			actualFields := make([]string, len(errorList))
			for i, validationError := range errorList {
				actualFields[i] = validationError.Field
			}
			require.ElementsMatch(t, testCase.expectedFields, actualFields, "%v", errorList)
		})
	}
}

// Verifies that LogicalContainerNetworkSpec.EffectiveMode defaults to session and preserves explicit modes.
func TestLogicalContainerNetworkSpecEffectiveMode(t *testing.T) {
	require.Equal(t, LogicalContainerNetworkModeSession, (LogicalContainerNetworkSpec{}).EffectiveMode())
	for _, mode := range []LogicalContainerNetworkMode{
		LogicalContainerNetworkModeSession, LogicalContainerNetworkModePersistent, LogicalContainerNetworkModeExisting,
	} {
		require.Equal(t, mode, (LogicalContainerNetworkSpec{Mode: mode}).EffectiveMode())
	}
}

// Verifies that LogicalContainerNetwork.Validate requires valid namespace metadata and bounded annotations.
func TestLogicalContainerNetworkValidateMetadata(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		namespace     string
		annotations   map[string]string
		expectedField string
	}{
		{"missing namespace", "", nil, "metadata.namespace"},
		{"invalid namespace", "Invalid_Namespace", nil, "metadata.namespace"},
		{"oversized annotations", "test-namespace", map[string]string{"large": strings.Repeat("a", commonapi.MaxAnnotationsTotalSize)}, "metadata.annotations"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			network := &LogicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: testCase.namespace, Annotations: testCase.annotations},
				Spec:       LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{}},
			}
			errorList := network.Validate(context.Background())
			require.NotEmpty(t, errorList)
			require.Equal(t, testCase.expectedField, errorList[0].Field)
		})
	}
}

// Verifies that LogicalContainerNetwork.ValidateUpdate freezes every spec field and permits status and metadata changes.
func TestLogicalContainerNetworkValidateUpdate(t *testing.T) {
	original := &LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test-namespace"},
		Spec: LogicalContainerNetworkSpec{
			Mode: LogicalContainerNetworkModePersistent, NetworkName: "runtime-network",
			Network: &LogicalContainerNetworkConfig{Labels: []commonapi.Label{{Key: "app", Value: "test"}}},
		},
	}
	for _, testCase := range []struct {
		name    string
		change  func(*LogicalContainerNetwork)
		allowed bool
	}{
		{"unchanged", func(*LogicalContainerNetwork) {}, true},
		{"status", func(network *LogicalContainerNetwork) { network.Status.Phase = LogicalContainerNetworkPhaseReady }, true},
		{"metadata", func(network *LogicalContainerNetwork) { network.Annotations = map[string]string{"key": "value"} }, true},
		{"name", func(network *LogicalContainerNetwork) { network.Spec.NetworkName = "different" }, false},
		{"mode", func(network *LogicalContainerNetwork) { network.Spec.Mode = LogicalContainerNetworkModeSession }, false},
		{"lifecycle key", func(network *LogicalContainerNetwork) { network.Spec.LifecycleKey = "different" }, false},
		{"config removed", func(network *LogicalContainerNetwork) { network.Spec.Network = nil }, false},
		{"ipv6", func(network *LogicalContainerNetwork) { network.Spec.Network.IPv6 = true }, false},
		{"labels", func(network *LogicalContainerNetwork) { network.Spec.Network.Labels[0].Value = "changed" }, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updated := original.DeepCopy()
			testCase.change(updated)
			errorList := updated.ValidateUpdate(context.Background(), original)
			if testCase.allowed {
				require.Empty(t, errorList)
			} else {
				require.Len(t, errorList, 1)
				require.Equal(t, "spec", errorList[0].Field)
			}
		})
	}
}

// Verifies that LogicalContainerNetworkSpec.GetLifecycleKey honors explicit keys, normalizes
// label ordering without mutating the spec, and includes IPv6 and label values in computed keys.
func TestLogicalContainerNetworkSpecGetLifecycleKey(t *testing.T) {
	spec := LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{
		Labels: []commonapi.Label{{Key: "second", Value: "2"}, {Key: "first", Value: "1"}},
	}}
	original := spec.DeepCopy()
	firstKey, computed, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.True(t, computed)
	require.NotEmpty(t, firstKey)
	require.Equal(t, *original, spec)
	spec.Network.Labels[0], spec.Network.Labels[1] = spec.Network.Labels[1], spec.Network.Labels[0]
	spec.NetworkName, spec.Mode = "another-runtime-name", LogicalContainerNetworkModePersistent
	reorderedKey, _, reorderedErr := spec.GetLifecycleKey()
	require.NoError(t, reorderedErr)
	require.Equal(t, firstKey, reorderedKey)
	spec.Network.IPv6 = true
	ipv6Key, _, ipv6Err := spec.GetLifecycleKey()
	require.NoError(t, ipv6Err)
	require.NotEqual(t, firstKey, ipv6Key)
	spec.Network.Labels[0].Value = "changed"
	changedKey, _, changedErr := spec.GetLifecycleKey()
	require.NoError(t, changedErr)
	require.NotEqual(t, ipv6Key, changedKey)

	explicit := LogicalContainerNetworkSpec{LifecycleKey: "caller-key"}
	explicitKey, explicitComputed, explicitErr := explicit.GetLifecycleKey()
	require.NoError(t, explicitErr)
	require.False(t, explicitComputed)
	require.Equal(t, "caller-key", explicitKey)
	missing := LogicalContainerNetworkSpec{}
	_, _, missingErr := missing.GetLifecycleKey()
	require.Error(t, missingErr)
}

// Verifies that LogicalContainerNetwork.GetLeaseKey shares runtime-name lease identity with V1
// networks, regardless of logical resource name or namespace.
func TestLogicalContainerNetworkGetLeaseKey(t *testing.T) {
	network := &LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: "namespace"},
		Spec:       LogicalContainerNetworkSpec{NetworkName: "runtime"},
	}
	require.Equal(t, "containernetworks/runtime", network.GetLeaseKey())
	network.Name, network.Namespace = "another", "other"
	require.Equal(t, "containernetworks/runtime", network.GetLeaseKey())
}

// Verifies that LogicalContainerNetwork prohibits creation during shutdown but permits deletion and status updates.
func TestLogicalContainerNetworkValidateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)
	network := &LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test-namespace"},
		Spec:       LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{}},
	}
	require.Contains(t, network.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	updated := network.DeepCopy()
	updated.Status.Phase = LogicalContainerNetworkPhaseReady
	require.Empty(t, updated.ValidateUpdate(context.Background(), network))
	deletionTime := metav1.Now()
	network.DeletionTimestamp = &deletionTime
	require.Empty(t, network.Validate(context.Background()))
}

// Verifies that LogicalContainerNetwork and its list register with standard storage and status interfaces.
func TestLogicalContainerNetworkRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	registered, registrationErr := scheme.New(GroupVersion.WithKind("LogicalContainerNetwork"))
	require.NoError(t, registrationErr)
	require.IsType(t, &LogicalContainerNetwork{}, registered)
	registeredList, listErr := scheme.New(GroupVersion.WithKind("LogicalContainerNetworkList"))
	require.NoError(t, listErr)
	require.IsType(t, &LogicalContainerNetworkList{}, registeredList)

	network := &LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test-namespace"}}
	require.Contains(t, PersistentTypes, &LogicalContainerNetwork{})
	require.Equal(t, GroupVersion.WithResource("logicalcontainernetworks"), network.GetGroupVersionResource())
	require.Equal(t, types.NamespacedName{Namespace: "test-namespace", Name: "network"}, network.NamespacedName())
	require.True(t, network.NamespaceScoped())
	require.True(t, network.IsStorageVersion())
	require.Equal(t, []string{"lcn"}, network.ShortNames())
	require.IsType(t, &LogicalContainerNetwork{}, network.New())
	require.IsType(t, &LogicalContainerNetworkList{}, network.NewList())
	require.Same(t, &network.ObjectMeta, network.GetObjectMeta())
	require.IsType(t, LogicalContainerNetworkStatus{}, network.GetStatus())

	list := &LogicalContainerNetworkList{Items: []LogicalContainerNetwork{*network}}
	require.Same(t, &list.ListMeta, list.GetListMeta())
	require.Equal(t, uint32(1), list.ItemCount())
	require.Same(t, &list.Items[0], list.GetItems()[0])
}

// Verifies that LogicalContainerNetwork JSON separates lifecycle policy from creation configuration
// and exposes observed network identity and its backing physical reference.
func TestLogicalContainerNetworkJSONShape(t *testing.T) {
	network := &LogicalContainerNetwork{
		TypeMeta:   metav1.TypeMeta{Kind: "LogicalContainerNetwork", APIVersion: GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test-namespace"},
		Spec: LogicalContainerNetworkSpec{
			NetworkName: "runtime-network", Mode: LogicalContainerNetworkModePersistent,
			Network: &LogicalContainerNetworkConfig{IPv6: true, Labels: []commonapi.Label{{Key: "app", Value: "test"}}},
		},
		Status: LogicalContainerNetworkStatus{
			Phase: LogicalContainerNetworkPhaseReady, PhysicalNetworkRef: "physical-network",
			NetworkID: "runtime-id", NetworkName: "runtime-network", Driver: "bridge", IPv6: true,
		},
	}
	encoded, marshalErr := json.Marshal(network)
	require.NoError(t, marshalErr)
	var fields struct {
		Spec   map[string]json.RawMessage `json:"spec"`
		Status map[string]json.RawMessage `json:"status"`
	}
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.JSONEq(t, `{"ipv6":true,"labels":[{"key":"app","value":"test"}]}`, string(fields.Spec["network"]))
	require.JSONEq(t, `"persistent"`, string(fields.Spec["mode"]))
	require.JSONEq(t, `"runtime-network"`, string(fields.Spec["networkName"]))
	require.NotContains(t, fields.Spec, "networkID")
	require.NotContains(t, fields.Spec, "persistent")
	require.NotContains(t, fields.Status, "message")
	require.JSONEq(t, `"physical-network"`, string(fields.Status["physicalNetworkRef"]))
	require.JSONEq(t, `"runtime-id"`, string(fields.Status["networkID"]))
	var restored LogicalContainerNetwork
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.Equal(t, network.Spec, restored.Spec)
	require.Equal(t, network.Status, restored.Status)
}

// Verifies that LogicalContainerNetwork.DeepCopy and status CopyTo isolate nested labels, address allocations,
// attached container IDs, and readiness conditions.
func TestLogicalContainerNetworkDeepCopy(t *testing.T) {
	network := &LogicalContainerNetwork{
		Spec: LogicalContainerNetworkSpec{Network: &LogicalContainerNetworkConfig{
			Labels: []commonapi.Label{{Key: "app", Value: "test"}},
		}},
		Status: LogicalContainerNetworkStatus{
			Subnets: []string{"10.0.0.0/24"}, Gateways: []string{"10.0.0.1"}, ContainerIDs: []string{"container-id"},
			Conditions: []metav1.Condition{{Type: string(ConditionReady), Message: "ready"}},
		},
	}
	copied := network.DeepCopy()
	require.Equal(t, network, copied)
	copied.Spec.Network.Labels[0].Value = "changed"
	copied.Status.Subnets[0] = "changed"
	require.Equal(t, "test", network.Spec.Network.Labels[0].Value)
	require.Equal(t, "10.0.0.0/24", network.Status.Subnets[0])
	destination := &LogicalContainerNetwork{}
	network.GetStatus().CopyTo(destination)
	destination.Status.Gateways[0] = "changed"
	destination.Status.ContainerIDs[0] = "changed"
	destination.Status.Conditions[0].Message = "changed"
	require.Equal(t, "10.0.0.1", network.Status.Gateways[0])
	require.Equal(t, "container-id", network.Status.ContainerIDs[0])
	require.Equal(t, "ready", network.Status.Conditions[0].Message)
}
