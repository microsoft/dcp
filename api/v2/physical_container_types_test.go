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

func TestPhysicalContainerValidate(t *testing.T) {
	testCases := []struct {
		name          string
		container     PhysicalContainer
		expectedError string
	}{
		{
			name: "valid created container",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
			},
		},
		{
			name: "valid existing container",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id",
					Stop:        true,
				},
			},
		},
		{
			name: "valid created container with port range",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort: 8080,
							RangeSize:     3,
							HostPort:      18080,
						},
					}},
				},
			},
		},
		{
			name: "missing namespace",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-container",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing container source",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
			},
			expectedError: "spec",
		},
		{
			name: "missing imageRef for created container",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{}},
			},
			expectedError: "spec.container.imageRef",
		},
		{
			name: "invalid imageRef",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "/"}},
			},
			expectedError: "spec.container.imageRef",
		},
		{
			name: "imageRef conflicts with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id", Container: &PhysicalContainerConfig{ImageRef: "test-image"},
				},
			},
			expectedError: "spec.container",
		},
		{
			name: "creation fields conflict with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id", Container: &PhysicalContainerConfig{Command: []string{"run"}},
				},
			},
			expectedError: "spec.container",
		},
		{
			name: "retain runtime container conflicts with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id", Container: &PhysicalContainerConfig{RetainRuntimeContainer: true},
				},
			},
			expectedError: "spec.container",
		},
		{
			name: "replaceExisting conflicts with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id", Container: &PhysicalContainerConfig{ReplaceExisting: true},
				},
			},
			expectedError: "spec.container",
		},
		{
			name: "replaceExisting requires containerName",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					ReplaceExisting: true},
				},
			},
			expectedError: "spec.container.containerName",
		},
		{
			name: "invalid container port range size",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort: 65535,
							RangeSize:     2,
						},
					}},
				},
			},
			expectedError: "spec.container.ports[0].rangeSize",
		},
		{
			name: "invalid host port range end",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort: 8080,
							RangeSize:     3,
							HostPort:      65534,
						},
					}},
				},
			},
			expectedError: "spec.container.ports[0].rangeSize",
		},
		{
			name: "unsupported port protocol",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort: 8080,
							Protocol:      commonapi.PortProtocol("SCTP"),
						},
					}},
				},
			},
			expectedError: "spec.container.ports[0].protocol",
		},
		{
			name: "invalid container name",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					ContainerName: "-invalid-container"},
				},
			},
			expectedError: "spec.container.containerName",
		},
		{
			name: "missing label key",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Labels: []commonapi.Label{
						{Value: "test-value"},
					}},
				},
			},
			expectedError: "spec.container.labels[0].key",
		},
		{
			name: "missing label value",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image",
					Labels: []commonapi.Label{
						{Key: "test-label"},
					}},
				},
			},
			expectedError: "spec.container.labels[0].value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errorList := tc.container.Validate(context.Background())
			if tc.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), tc.expectedError)
			}
		})
	}
}

func TestPhysicalContainerValidateUpdateRejectsSpecChanges(t *testing.T) {
	oldContainer := &PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-container",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Spec.Container.ImageRef = "different-image"

	errorList := newContainer.ValidateUpdate(context.Background(), oldContainer)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec")
}

func TestPhysicalContainerValidateUpdateAllowsStopRequest(t *testing.T) {
	oldContainer := &PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-container",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Spec.Stop = true

	errorList := newContainer.ValidateUpdate(context.Background(), oldContainer)

	require.Empty(t, errorList)
}

func TestPhysicalContainerValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	oldContainer := &PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-container",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Status.Phase = PhysicalContainerPhaseRunning

	validationErr := newContainer.Validate(context.Background()).ToAggregate()
	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())

	errorList := newContainer.ValidateUpdate(context.Background(), oldContainer)

	require.Empty(t, errorList)
}

func TestPhysicalContainerValidateUpdateRejectsClearingStop(t *testing.T) {
	oldContainer := &PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-container",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerSpec{

			Stop: true, Container: &PhysicalContainerConfig{ImageRef: "test-image"},
		},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Spec.Stop = false

	errorList := newContainer.ValidateUpdate(context.Background(), oldContainer)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec.stop")
}
