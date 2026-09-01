/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

const (
	// V1PhysicalResourcesNamespaceName is the shared V2 namespace for physical resources created by V1 controllers.
	V1PhysicalResourcesNamespaceName = "v1-compatibility"
)

// EnsureV1PhysicalResourcesNamespace creates the shared V2 namespace used by V1 controllers.
func EnsureV1PhysicalResourcesNamespace(ctx context.Context, client ctrl_client.Client) error {
	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: V1PhysicalResourcesNamespaceName,
		},
	}
	createErr := client.Create(ctx, namespace)
	if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
		return fmt.Errorf("create V1 physical resources namespace: %w", createErr)
	}

	return nil
}
