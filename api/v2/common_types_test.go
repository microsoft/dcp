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
	"k8s.io/apimachinery/pkg/util/validation/field"

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

func TestValidateSameNamespaceResourceReference(t *testing.T) {
	testCases := []struct {
		name          string
		reference     string
		expectedError string
	}{
		{
			name:      "name",
			reference: "test-resource",
		},
		{
			name:      "explicit same namespace",
			reference: "test-namespace/test-resource",
		},
		{
			name:          "missing reference",
			expectedError: "reference must be set",
		},
		{
			name:          "cross namespace",
			reference:     "other-namespace/test-resource",
			expectedError: "cross-namespace references are not supported",
		},
		{
			name:          "missing explicit namespace",
			reference:     "/test-resource",
			expectedError: "lowercase RFC 1123 label",
		},
		{
			name:          "missing explicit name",
			reference:     "test-namespace/",
			expectedError: "lowercase RFC 1123 subdomain",
		},
		{
			name:          "additional separator",
			reference:     "test-namespace/test-resource/extra",
			expectedError: "lowercase RFC 1123 subdomain",
		},
		{
			name:          "invalid explicit namespace",
			reference:     "INVALID/test-resource",
			expectedError: "lowercase RFC 1123 label",
		},
		{
			name:          "invalid name",
			reference:     "INVALID",
			expectedError: "lowercase RFC 1123 subdomain",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errorList := validateSameNamespaceResourceReference(
				tc.reference,
				"test-namespace",
				field.NewPath("spec", "reference"),
			)
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
					Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-image"}},
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
					Spec: PhysicalContainerSpec{Container: &PhysicalContainerConfig{ImageRef: "test-image"}},
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
