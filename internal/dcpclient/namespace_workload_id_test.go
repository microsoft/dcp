/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpclient

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that ResolveNamespaceWorkloadID selects a namespace override when present
// and otherwise falls back to the default workload ID.
func TestResolveNamespaceWorkloadIDSelectsOverrideOrDefault(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		annotations map[string]string
		expected    commonapi.WorkloadID
	}{
		{
			name:        "namespace override",
			annotations: map[string]string{apiv2.NamespaceWorkloadIDAnnotation: " namespace-workload "},
			expected:    "namespace-workload",
		},
		{
			name:     "default fallback",
			expected: "global-workload",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().
				WithScheme(NewScheme()).
				WithObjects(&apiv2.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-namespace",
						Annotations: testCase.annotations,
					},
				}).
				Build()

			workloadID, resolveErr := ResolveNamespaceWorkloadID(t.Context(), reader, "test-namespace", " global-workload ")
			require.NoError(t, resolveErr)
			require.Equal(t, testCase.expected, workloadID)
		})
	}
}
