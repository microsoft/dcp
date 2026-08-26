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

func TestPhysicalContainerImageValidate(t *testing.T) {
	testCases := []struct {
		name          string
		image         PhysicalContainerImage
		expectedError string
	}{
		{
			name: "valid existing image",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{ImageID: "existing-image-id"},
			},
		},
		{
			name: "valid source image",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-source-image"}},
			},
		},
		{
			name: "valid build image without explicit target tag",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
				}},
				},
			},
		},
		{
			name: "valid build image with explicit target tag",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-target-image", Build: &ContainerBuildContext{
					Context: "test-context",
				}},
				},
			},
		},
		{
			name: "valid build image with file secret",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Secrets: []ContainerBuildSecret{
						{
							Type:   FileSecret,
							ID:     "test-secret",
							Source: "test-secret-file",
						},
					},
				}},
				},
			},
		},
		{
			name: "valid build image with env secret without source",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Secrets: []ContainerBuildSecret{
						{
							Type: EnvSecret,
							ID:   "test-secret",
						},
					},
				}},
				},
			},
		},
		{
			name: "missing namespace",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-image",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-source-image"}},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing image source",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
			},
			expectedError: "spec",
		},
		{
			name: "missing image and build",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{}},
			},
			expectedError: "spec.image.image",
		},
		{
			name: "image definition conflicts with existing image ID",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{
					ImageID: "existing-image-id",
					Image:   &PhysicalContainerImageConfig{Image: "test-source-image"},
				},
			},
			expectedError: "spec.image",
		},
		{
			name: "invalid pull policy",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-source-image", PullPolicy: "invalid"}},
			},
			expectedError: "spec.image.pullPolicy",
		},
		{
			name: "never pull policy with build",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
				},
					PullPolicy: PullPolicyNever},
				},
			},
			expectedError: "spec.image.pullPolicy",
		},
		{
			name: "missing build context",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{}}},
			},
			expectedError: "spec.image.build.context",
		},
		{
			name: "missing build file secret source",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Secrets: []ContainerBuildSecret{
						{
							Type: FileSecret,
							ID:   "test-secret",
						},
					},
				}},
				},
			},
			expectedError: "spec.image.build.secrets[0].source",
		},
		{
			name: "missing build default file secret source",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Secrets: []ContainerBuildSecret{
						{
							ID: "test-secret",
						},
					},
				}},
				},
			},
			expectedError: "spec.image.build.secrets[0].source",
		},
		{
			name: "missing build label key",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Labels: []commonapi.Label{
						{Value: "test-value"},
					},
				}},
				},
			},
			expectedError: "spec.image.build.labels[0].key",
		},
		{
			name: "missing build label value",
			image: PhysicalContainerImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-image",
					Namespace: "test-namespace",
				},
				Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
					Context: "test-context",
					Labels: []commonapi.Label{
						{Key: "test-label"},
					},
				}},
				},
			},
			expectedError: "spec.image.build.labels[0].value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errorList := tc.image.Validate(context.Background())
			if tc.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), tc.expectedError)
			}
		})
	}
}

func TestPhysicalContainerImageValidateUpdateRejectsSpecChanges(t *testing.T) {
	oldImage := &PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-image",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-source-image"}},
	}
	newImage := oldImage.DeepCopy()
	newImage.Spec.Image.Image = "different-source-image"

	errorList := newImage.ValidateUpdate(context.Background(), oldImage)

	require.NotEmpty(t, errorList)
	require.Contains(t, errorList.ToAggregate().Error(), "spec")
}

func TestPhysicalContainerImageValidateUpdateAllowsStatusUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)

	oldImage := &PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-image",
			Namespace: "test-namespace",
		},
		Spec: PhysicalContainerImageSpec{Image: &PhysicalContainerImageConfig{Image: "test-source-image"}},
	}
	newImage := oldImage.DeepCopy()
	newImage.Status.Phase = PhysicalContainerImagePhaseReady

	validationErr := newImage.Validate(context.Background()).ToAggregate()
	require.Error(t, validationErr)
	require.Contains(t, validationErr.Error(), commonapi.ErrResourceCreationProhibited.Error())

	errorList := newImage.ValidateUpdate(context.Background(), oldImage)

	require.Empty(t, errorList)
}
