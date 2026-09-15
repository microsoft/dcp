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
)

func TestPhysicalContainerNetworkConnectionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		connection PhysicalContainerNetworkConnection
		valid      bool
	}{
		{
			name: "valid",
			connection: PhysicalContainerNetworkConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "namespace"},
				Spec: PhysicalContainerNetworkConnectionSpec{
					ContainerRef: "container",
					NetworkRef:   "network",
					Aliases:      []string{"alias"},
				},
			},
			valid: true,
		},
		{
			name: "missing container reference",
			connection: PhysicalContainerNetworkConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "namespace"},
				Spec: PhysicalContainerNetworkConnectionSpec{
					NetworkRef: "network",
				},
			},
		},
		{
			name: "invalid container reference",
			connection: PhysicalContainerNetworkConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "namespace"},
				Spec: PhysicalContainerNetworkConnectionSpec{
					ContainerRef: "INVALID",
					NetworkRef:   "network",
				},
			},
		},
		{
			name: "missing network reference",
			connection: PhysicalContainerNetworkConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "namespace"},
				Spec: PhysicalContainerNetworkConnectionSpec{
					ContainerRef: "container",
				},
			},
		},
		{
			name: "invalid network reference",
			connection: PhysicalContainerNetworkConnection{
				ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "namespace"},
				Spec: PhysicalContainerNetworkConnectionSpec{
					ContainerRef: "container",
					NetworkRef:   "INVALID",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			errorList := test.connection.Validate(context.Background())
			if test.valid {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
			}
		})
	}
}

func TestPhysicalContainerNetworkConnectionValidateUpdate(t *testing.T) {
	t.Parallel()

	withoutAliases := &PhysicalContainerNetworkConnection{
		Spec: PhysicalContainerNetworkConnectionSpec{
			ContainerRef: "container",
			NetworkRef:   "network",
		},
	}
	emptyAliasesUpdate := withoutAliases.DeepCopy()
	emptyAliasesUpdate.Spec.Aliases = []string{}
	require.Empty(t, emptyAliasesUpdate.ValidateUpdate(context.Background(), withoutAliases))

	original := &PhysicalContainerNetworkConnection{
		Spec: PhysicalContainerNetworkConnectionSpec{
			ContainerRef: "container",
			NetworkRef:   "network",
			Aliases:      []string{"alias"},
		},
	}

	specUpdate := original.DeepCopy()
	specUpdate.Spec.NetworkRef = "other-network"
	require.NotEmpty(t, specUpdate.ValidateUpdate(context.Background(), original))
}
