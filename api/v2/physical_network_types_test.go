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

func TestPhysicalNetworkValidate(t *testing.T) {
	testCases := []struct {
		name          string
		network       PhysicalNetwork
		expectedError string
	}{
		{
			name: "valid created network",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "test-runtime-network",
				},
			},
		},
		{
			name: "valid created network with labels and IPv6",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "test-runtime-network",
					IPv6:        true,
					Labels: []commonapi.Label{
						{Key: "test-label", Value: "test-value"},
					},
				},
			},
		},
		{
			// Podman allows a single-character network name, unlike the Docker container name pattern.
			name: "valid single character network name",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "a",
				},
			},
		},
		{
			name: "valid tracked network",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkID: "test-network-id",
				},
			},
		},
		{
			name: "valid tracked network preserved on deletion",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkID:          "test-network-id",
					PreserveOnDeletion: true,
				},
			},
		},
		{
			name: "missing namespace",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-network",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "test-runtime-network",
				},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing network name",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
			},
			expectedError: "spec.networkName",
		},
		{
			name: "invalid network name",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "-invalid-network",
				},
			},
			expectedError: "spec.networkName",
		},
		{
			name: "network name with whitespace",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "invalid network",
				},
			},
			expectedError: "spec.networkName",
		},
		{
			name: "network name with tracked network",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkID:   "test-network-id",
					NetworkName: "test-runtime-network",
				},
			},
			expectedError: "spec.networkName",
		},
		{
			name: "IPv6 with tracked network",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkID: "test-network-id",
					IPv6:      true,
				},
			},
			expectedError: "spec.ipv6",
		},
		{
			name: "labels with tracked network",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkID: "test-network-id",
					Labels: []commonapi.Label{
						{Key: "test-label", Value: "test-value"},
					},
				},
			},
			expectedError: "spec.labels",
		},
		{
			name: "missing label key",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "test-runtime-network",
					Labels: []commonapi.Label{
						{Value: "test-value"},
					},
				},
			},
			expectedError: "spec.labels[0].key",
		},
		{
			name: "missing label value",
			network: PhysicalNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalNetworkSpec{
					NetworkName: "test-runtime-network",
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
			errorList := tc.network.Validate(context.Background())
			if tc.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), tc.expectedError)
			}
		})
	}
}

func TestPhysicalNetworkValidateRejectsCreationDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	network := &PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalNetworkSpec{
			NetworkName: "test-runtime-network",
		},
	}

	validationErr := network.Validate(context.Background()).ToAggregate()

	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())
}

func TestPhysicalNetworkValidateAllowsDeletionDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	deletionTimestamp := metav1.Now()
	network := &PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-network",
			Namespace:         "test-namespace",
			DeletionTimestamp: &deletionTimestamp,
		},
		Spec: PhysicalNetworkSpec{
			NetworkName: "test-runtime-network",
		},
	}

	require.Empty(t, network.Validate(context.Background()))
}

func TestPhysicalNetworkValidateUpdateRejectsSpecChanges(t *testing.T) {
	oldNetwork := &PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalNetworkSpec{
			NetworkName: "test-runtime-network",
		},
	}
	newNetwork := oldNetwork.DeepCopy()
	newNetwork.Spec.NetworkName = "different-runtime-network"

	errorList := newNetwork.ValidateUpdate(context.Background(), oldNetwork)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec")
}

func TestPhysicalNetworkValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	oldNetwork := &PhysicalNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalNetworkSpec{
			NetworkName: "test-runtime-network",
		},
	}
	newNetwork := oldNetwork.DeepCopy()
	newNetwork.Status.Phase = PhysicalNetworkPhaseReady

	validationErr := newNetwork.Validate(context.Background()).ToAggregate()
	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())

	errorList := newNetwork.ValidateUpdate(context.Background(), oldNetwork)

	require.Empty(t, errorList)
}
