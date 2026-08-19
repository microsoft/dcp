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
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
				},
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
					Persistent:  true,
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
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort:    8080,
							ContainerPortEnd: 8082,
							HostPort:         18080,
						},
					},
				},
			},
		},
		{
			name: "missing namespace",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-container",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
				},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing imageRef for created container",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
			},
			expectedError: "spec.imageRef",
		},
		{
			name: "invalid imageRef",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "/",
				},
			},
			expectedError: "spec.imageRef",
		},
		{
			name: "imageRef conflicts with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id",
					ImageRef:    "test-image",
				},
			},
			expectedError: "spec.imageRef",
		},
		{
			name: "creation fields conflict with existing container ID",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ContainerID: "existing-container-id",
					Command:     []string{"run"},
				},
			},
			expectedError: "spec.command",
		},
		{
			name: "invalid container port range end",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort:    8080,
							ContainerPortEnd: 8079,
						},
					},
				},
			},
			expectedError: "spec.ports[0].containerPortEnd",
		},
		{
			name: "invalid host port range end",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort:    8080,
							ContainerPortEnd: 8082,
							HostPort:         65534,
						},
					},
				},
			},
			expectedError: "spec.ports[0].hostPort",
		},
		{
			name: "unsupported port protocol",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Ports: []ContainerPort{
						{
							ContainerPort: 8080,
							Protocol:      commonapi.PortProtocol("SCTP"),
						},
					},
				},
			},
			expectedError: "spec.ports[0].protocol",
		},
		{
			name: "invalid container name",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef:      "test-image",
					ContainerName: "-invalid-container",
				},
			},
			expectedError: "spec.containerName",
		},
		{
			name: "missing label key",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Labels: []commonapi.Label{
						{Value: "test-value"},
					},
				},
			},
			expectedError: "spec.labels[0].key",
		},
		{
			name: "missing label value",
			container: PhysicalContainer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-container",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerSpec{
					ImageRef: "test-image",
					Labels: []commonapi.Label{
						{Key: "test-label"},
					},
				},
			},
			expectedError: "spec.labels[0].value",
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
		Spec: PhysicalContainerSpec{
			ImageRef: "test-image",
		},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Spec.ImageRef = "different-image"

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
		Spec: PhysicalContainerSpec{
			ImageRef: "test-image",
		},
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
		Spec: PhysicalContainerSpec{
			ImageRef: "test-image",
		},
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
			ImageRef: "test-image",
			Stop:     true,
		},
	}
	newContainer := oldContainer.DeepCopy()
	newContainer.Spec.Stop = false

	errorList := newContainer.ValidateUpdate(context.Background(), oldContainer)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec.stop")
}
