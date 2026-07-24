/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestNamespaceValidate(t *testing.T) {
	testCases := []struct {
		name          string
		namespace     Namespace
		expectedError string
	}{
		{
			name: "valid namespace",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-namespace",
					Annotations: map[string]string{
						NamespaceWorkloadIDAnnotation: "test-workload",
					},
				},
			},
		},
		{
			name: "empty name is handled by generic metadata validation",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{},
			},
		},
		{
			name: "invalid name",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "Invalid_Namespace",
				},
			},
			expectedError: "metadata.name",
		},
		{
			name: "metadata namespace is forbidden",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-namespace",
					Namespace: "parent-namespace",
				},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "annotations are too large",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-namespace",
					Annotations: map[string]string{
						"large-value": strings.Repeat("a", commonapi.MaxAnnotationsTotalSize),
					},
				},
			},
			expectedError: "metadata.annotations",
		},
		{
			name: "workload ID is too long",
			namespace: Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-namespace",
					Annotations: map[string]string{
						NamespaceWorkloadIDAnnotation: strings.Repeat("a", commonapi.MaxWorkloadIDLength+1),
					},
				},
			},
			expectedError: NamespaceWorkloadIDAnnotation,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errorList := tc.namespace.Validate(context.Background())
			if tc.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), tc.expectedError)
			}
		})
	}
}
