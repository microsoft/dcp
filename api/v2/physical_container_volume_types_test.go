/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestPhysicalContainerVolumeValidate(t *testing.T) {
	testCases := []struct {
		name          string
		volume        PhysicalContainerVolume
		expectedError string
	}{
		{
			name: "valid created volume",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{
						VolumeName: "test-runtime-volume",
						Labels:     []commonapi.Label{{Key: "test-label", Value: "test-value"}},
					},
				},
			},
		},
		{
			name: "valid tracked volume",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec:       PhysicalContainerVolumeSpec{VolumeID: "test-runtime-volume"},
			},
		},
		{
			name: "valid replacement volume",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{
						VolumeName:      "test-runtime-volume",
						ReplaceExisting: true,
					},
				},
			},
		},
		{
			name: "missing namespace",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{VolumeName: "test-runtime-volume"},
				},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing volume source",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
			},
			expectedError: "spec",
		},
		{
			name: "volume ID and config",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					VolumeID: "test-runtime-volume",
					Volume:   &PhysicalContainerVolumeConfig{VolumeName: "another-runtime-volume"},
				},
			},
			expectedError: "spec.volume",
		},
		{
			name: "missing volume name",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{},
				},
			},
			expectedError: "spec.volume.volumeName",
		},
		{
			name: "whitespace-only volume name",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{VolumeName: " "},
				},
			},
			expectedError: "spec.volume.volumeName",
		},
		{
			name: "missing label key",
			volume: PhysicalContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
				Spec: PhysicalContainerVolumeSpec{
					Volume: &PhysicalContainerVolumeConfig{
						VolumeName: "test-runtime-volume",
						Labels:     []commonapi.Label{{Value: "test-value"}},
					},
				},
			},
			expectedError: "spec.volume.labels[0].key",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errorList := testCase.volume.Validate(context.Background())
			if testCase.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), testCase.expectedError)
			}
		})
	}
}

func TestPhysicalContainerVolumeValidateUpdateRejectsSpecChanges(t *testing.T) {
	oldVolume := &PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
		Spec: PhysicalContainerVolumeSpec{
			Volume: &PhysicalContainerVolumeConfig{VolumeName: "test-runtime-volume"},
		},
	}
	newVolume := oldVolume.DeepCopy()
	newVolume.Spec.Volume.RetainRuntimeVolume = true

	errorList := newVolume.ValidateUpdate(context.Background(), oldVolume)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec")
}

func TestPhysicalContainerVolumeValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	oldVolume := &PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-volume", Namespace: "test-namespace"},
		Spec: PhysicalContainerVolumeSpec{
			Volume: &PhysicalContainerVolumeConfig{VolumeName: "test-runtime-volume"},
		},
	}
	newVolume := oldVolume.DeepCopy()
	newVolume.Status.Phase = PhysicalContainerVolumePhaseReady

	require.Error(t, newVolume.Validate(context.Background()).ToAggregate())
	require.Empty(t, newVolume.ValidateUpdate(context.Background(), oldVolume))
}
