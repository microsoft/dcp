/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that PhysicalContainerVolume.Validate requires a valid runtime volume name and labels.
func TestPhysicalContainerVolumeValidate(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name          string
		spec          PhysicalContainerVolumeSpec
		expectedError string
	}{
		{name: "retained", spec: PhysicalContainerVolumeSpec{VolumeName: "test-runtime-volume"}},
		{name: "removable", spec: PhysicalContainerVolumeSpec{VolumeName: "test-runtime-volume", RemoveRuntimeVolumeOnDelete: true}},
		{name: "labelled", spec: PhysicalContainerVolumeSpec{
			VolumeName: "test-runtime-volume", Labels: []commonapi.Label{{Key: "test-label", Value: "test-value"}},
		}},
		{name: "missing name", expectedError: "spec.volumeName"},
		{name: "whitespace name", spec: PhysicalContainerVolumeSpec{VolumeName: " "}, expectedError: "spec.volumeName"},
		{name: "invalid name", spec: PhysicalContainerVolumeSpec{VolumeName: "invalid volume"}, expectedError: "spec.volumeName"},
		{name: "single character", spec: PhysicalContainerVolumeSpec{VolumeName: "v"}, expectedError: "spec.volumeName"},
		{name: "missing label key", spec: PhysicalContainerVolumeSpec{
			VolumeName: "test-runtime-volume", Labels: []commonapi.Label{{Value: "test-value"}},
		}, expectedError: "spec.labels[0].key"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			volume := &PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: "test-namespace"},
				Spec:       testCase.spec,
			}
			errorList := volume.Validate(context.Background())
			if testCase.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), testCase.expectedError)
			}
		})
	}
	volume := &PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "volume"},
		Spec:       PhysicalContainerVolumeSpec{VolumeName: "runtime-volume"},
	}
	require.Contains(t, volume.Validate(context.Background()).ToAggregate().Error(), "metadata.namespace")
}

// Verifies that PhysicalContainerVolume.ValidateUpdate keeps every top-level spec field immutable.
func TestPhysicalContainerVolumeValidateUpdateRejectsSpecChanges(t *testing.T) {
	t.Parallel()
	original := &PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: "test-namespace"},
		Spec:       PhysicalContainerVolumeSpec{VolumeName: "runtime-volume"},
	}
	for _, testCase := range []struct {
		name   string
		change func(*PhysicalContainerVolume)
	}{
		{name: "name", change: func(volume *PhysicalContainerVolume) { volume.Spec.VolumeName = "another-volume" }},
		{name: "cleanup", change: func(volume *PhysicalContainerVolume) { volume.Spec.RemoveRuntimeVolumeOnDelete = true }},
		{name: "labels", change: func(volume *PhysicalContainerVolume) {
			volume.Spec.Labels = []commonapi.Label{{Key: "key", Value: "value"}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updated := original.DeepCopy()
			testCase.change(updated)
			updateErrors := updated.ValidateUpdate(context.Background(), original)
			require.Len(t, updateErrors, 1)
			require.Equal(t, "spec", updateErrors[0].Field)
		})
	}
}

// Verifies that PhysicalContainerVolume.ValidateUpdate permits status updates during shutdown.
func TestPhysicalContainerVolumeValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)
	original := &PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "volume", Namespace: "test-namespace"},
		Spec:       PhysicalContainerVolumeSpec{VolumeName: "runtime-volume"},
	}
	updated := original.DeepCopy()
	updated.Status.Phase = PhysicalContainerVolumePhaseReady
	require.Error(t, updated.Validate(context.Background()).ToAggregate())
	require.Empty(t, updated.ValidateUpdate(context.Background(), original))
}

// Verifies that PhysicalContainerVolume serializes a flat spec, retains observed volumeID in status,
// and deep copies isolate the configured labels.
func TestPhysicalContainerVolumeJSONAndDeepCopy(t *testing.T) {
	t.Parallel()
	volume := &PhysicalContainerVolume{
		Spec: PhysicalContainerVolumeSpec{
			VolumeName: "runtime-volume", RemoveRuntimeVolumeOnDelete: true,
			Labels: []commonapi.Label{{Key: "key", Value: "value"}},
		},
		Status: PhysicalContainerVolumeStatus{VolumeID: "runtime-volume"},
	}
	encoded, encodeErr := json.Marshal(volume)
	require.NoError(t, encodeErr)
	var document struct {
		Spec   map[string]json.RawMessage `json:"spec"`
		Status map[string]json.RawMessage `json:"status"`
	}
	require.NoError(t, json.Unmarshal(encoded, &document))
	require.Len(t, document.Spec, 3)
	require.JSONEq(t, `"runtime-volume"`, string(document.Spec["volumeName"]))
	require.JSONEq(t, `true`, string(document.Spec["removeRuntimeVolumeOnDelete"]))
	require.JSONEq(t, `[{"key":"key","value":"value"}]`, string(document.Spec["labels"]))
	require.JSONEq(t, `"runtime-volume"`, string(document.Status["volumeID"]))
	cloned := volume.DeepCopy()
	cloned.Spec.Labels[0].Value = "changed"
	require.Equal(t, "value", volume.Spec.Labels[0].Value)
}
