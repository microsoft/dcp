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
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestNamespacedName(t *testing.T) {
	obj := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-object",
			Namespace: "test-namespace",
		},
	}

	require.Equal(t, types.NamespacedName{Namespace: "test-namespace", Name: "test-object"}, NamespacedName(obj))
}

func TestValidateNamespacedResourceMetadata(t *testing.T) {
	testCases := []struct {
		name          string
		namespace     string
		expectedError string
	}{
		{
			name:      "valid namespace",
			namespace: "test-namespace",
		},
		{
			name:          "missing namespace",
			expectedError: "metadata.namespace",
		},
		{
			name:          "invalid namespace",
			namespace:     "Invalid_Namespace",
			expectedError: "metadata.namespace",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &metav1.PartialObjectMetadata{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-object",
					Namespace: tc.namespace,
				},
			}

			errorList := ValidateNamespacedResourceMetadata(obj)
			if tc.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), tc.expectedError)
			}
		})
	}
}

func TestResourceCreationProhibited(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	testCases := []struct {
		name     string
		validate func() error
	}{
		{
			name: "namespace",
			validate: func() error {
				namespace := &Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "test-namespace"},
				}
				return namespace.Validate(context.Background()).ToAggregate()
			},
		},
		{
			name: "physical container image",
			validate: func() error {
				image := &PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-image",
						Namespace: "test-namespace",
					},
					Spec: PhysicalContainerImageSpec{
						Image: "test-image",
					},
				}
				return image.Validate(context.Background()).ToAggregate()
			},
		},
		{
			name: "physical container",
			validate: func() error {
				container := &PhysicalContainer{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-container",
						Namespace: "test-namespace",
					},
					Spec: PhysicalContainerSpec{
						ImageRef: "test-image",
					},
				}
				return container.Validate(context.Background()).ToAggregate()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			validationErr := tc.validate()
			require.Error(t, validationErr)
			require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())
		})
	}
}
