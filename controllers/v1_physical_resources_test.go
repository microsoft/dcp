/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client_fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestEnsureV1PhysicalResourcesNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	client := ctrl_client_fake.NewClientBuilder().WithScheme(scheme).Build()

	require.NoError(t, EnsureV1PhysicalResourcesNamespace(context.Background(), client))
	require.NoError(t, EnsureV1PhysicalResourcesNamespace(context.Background(), client))

	namespace := apiv2.Namespace{}
	getErr := client.Get(context.Background(), types.NamespacedName{Name: V1PhysicalResourcesNamespaceName}, &namespace)
	require.NoError(t, getErr)
	require.Equal(t, V1PhysicalResourcesNamespaceName, namespace.Name)
}
