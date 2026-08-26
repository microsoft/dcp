/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpclient

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestResolveNamespaceWorkloadIDUsesNamespaceAnnotation(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(&apiv2.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Annotations: map[string]string{
					apiv2.NamespaceWorkloadIDAnnotation: " namespace-workload ",
				},
			},
		}).
		Build()

	workloadID, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "test-namespace", "global-workload")
	require.NoError(t, resolveErr)
	require.Equal(t, commonapi.WorkloadID("namespace-workload"), workloadID)
}

func TestResolveNamespaceWorkloadIDFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(&apiv2.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
			},
		}).
		Build()

	workloadID, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "test-namespace", " global-workload ")
	require.NoError(t, resolveErr)
	require.Equal(t, commonapi.WorkloadID("global-workload"), workloadID)
}

func TestResolveNamespaceWorkloadIDRequiresNamespaceName(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().WithScheme(NewScheme()).Build()

	_, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "", "global-workload")
	require.Error(t, resolveErr)
	require.Contains(t, resolveErr.Error(), "namespace name is required")
}

func TestResolveNamespaceWorkloadIDReturnsMissingNamespaceError(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().WithScheme(NewScheme()).Build()

	_, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "missing-namespace", "global-workload")
	require.Error(t, resolveErr)
	require.Contains(t, resolveErr.Error(), "missing-namespace")
}

func TestResolveNamespaceWorkloadIDValidatesNamespaceAnnotation(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(&apiv2.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Annotations: map[string]string{
					apiv2.NamespaceWorkloadIDAnnotation: strings.Repeat("a", commonapi.MaxWorkloadIDLength+1),
				},
			},
		}).
		Build()

	_, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "test-namespace", "global-workload")
	require.Error(t, resolveErr)
	require.Contains(t, resolveErr.Error(), apiv2.NamespaceWorkloadIDAnnotation)
}

func TestResolveNamespaceWorkloadIDValidatesDefault(t *testing.T) {
	ctx := context.Background()
	reader := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(&apiv2.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
			},
		}).
		Build()

	_, resolveErr := ResolveNamespaceWorkloadID(ctx, reader, "test-namespace", commonapi.WorkloadID(strings.Repeat("a", commonapi.MaxWorkloadIDLength+1)))
	require.Error(t, resolveErr)
	require.Contains(t, resolveErr.Error(), "default workload ID")
}
