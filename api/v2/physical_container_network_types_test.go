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

func TestPhysicalContainerNetworkValidate(t *testing.T) {
	testCases := []struct {
		name          string
		network       PhysicalContainerNetwork
		expectedError string
	}{
		{
			name: "valid created network",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "test-runtime-network",
					},
				},
			},
		},
		{
			name: "valid created network with labels and IPv6",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "test-runtime-network",
						IPv6:        true,
						Labels: []commonapi.Label{
							{Key: "test-label", Value: "test-value"},
						},
					},
				},
			},
		},
		{
			// Podman allows a single-character network name, unlike the Docker container name pattern.
			name: "valid single character network name",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "a",
					},
				},
			},
		},
		{
			name: "valid tracked network",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					NetworkID: "test-network-id",
				},
			},
		},
		{
			name: "valid replacement network",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName:     "test-runtime-network",
						ReplaceExisting: true,
					},
				},
			},
		},
		{
			name: "missing namespace",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-network",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "test-runtime-network",
					},
				},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing network ID and create definition",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
			},
			expectedError: "spec",
		},
		{
			name: "missing network name",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{},
				},
			},
			expectedError: "spec.network.networkName",
		},
		{
			name: "invalid network name",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "-invalid-network",
					},
				},
			},
			expectedError: "spec.network.networkName",
		},
		{
			name: "network name with whitespace",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "invalid network",
					},
				},
			},
			expectedError: "spec.network.networkName",
		},
		{
			name: "create definition with tracked network",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					NetworkID: "test-network-id",
					Network: &PhysicalContainerNetworkConfig{
						NetworkName:          "test-runtime-network",
						RetainRuntimeNetwork: true,
						ReplaceExisting:      true,
						IPv6:                 true,
						Labels: []commonapi.Label{
							{Key: "test-label", Value: "test-value"},
						},
					},
				},
			},
			expectedError: "spec.network",
		},
		{
			name: "missing label key",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "test-runtime-network",
						Labels: []commonapi.Label{
							{Value: "test-value"},
						},
					},
				},
			},
			expectedError: "spec.network.labels[0].key",
		},
		{
			name: "missing label value",
			network: PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerNetworkSpec{
					Network: &PhysicalContainerNetworkConfig{
						NetworkName: "test-runtime-network",
						Labels: []commonapi.Label{
							{Key: "test-label"},
						},
					},
				},
			},
			expectedError: "spec.network.labels[0].value",
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

func TestPhysicalContainerNetworkValidateRejectsCreationDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	network := &PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerNetworkSpec{
			Network: &PhysicalContainerNetworkConfig{
				NetworkName: "test-runtime-network",
			},
		},
	}

	validationErr := network.Validate(context.Background()).ToAggregate()

	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())
}

func TestPhysicalContainerNetworkValidateAllowsDeletionDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	deletionTimestamp := metav1.Now()
	network := &PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-network",
			Namespace:         "test-namespace",
			DeletionTimestamp: &deletionTimestamp,
		},
		Spec: PhysicalContainerNetworkSpec{
			Network: &PhysicalContainerNetworkConfig{
				NetworkName: "test-runtime-network",
			},
		},
	}

	require.Empty(t, network.Validate(context.Background()))
}

func TestPhysicalContainerNetworkValidateUpdateRejectsSpecChanges(t *testing.T) {
	oldNetwork := &PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerNetworkSpec{
			Network: &PhysicalContainerNetworkConfig{
				NetworkName: "test-runtime-network",
			},
		},
	}
	newNetwork := oldNetwork.DeepCopy()
	newNetwork.Spec.Network.NetworkName = "different-runtime-network"

	errorList := newNetwork.ValidateUpdate(context.Background(), oldNetwork)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec")
}

func TestPhysicalContainerNetworkValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	oldNetwork := &PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-network",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerNetworkSpec{
			Network: &PhysicalContainerNetworkConfig{
				NetworkName: "test-runtime-network",
			},
		},
	}
	newNetwork := oldNetwork.DeepCopy()
	newNetwork.Status.Phase = PhysicalContainerNetworkPhaseReady

	validationErr := newNetwork.Validate(context.Background()).ToAggregate()
	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())

	errorList := newNetwork.ValidateUpdate(context.Background(), oldNetwork)

	require.Empty(t, errorList)
}
