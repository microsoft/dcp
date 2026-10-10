/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"

	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

func disableV1CollectionDeletion(config *tiltapiserver.Config) error {
	for _, resource := range apiv1.PersistentTypes {
		gvr := resource.GetGroupVersionResource()
		provider, found := config.ExtraConfig.APIs[gvr]
		if !found || provider == nil {
			return fmt.Errorf("missing V1 storage provider for %s", gvr)
		}
		config.ExtraConfig.APIs[gvr] = withCollectionDeletionDisabled(provider, gvr)
	}
	return nil
}

func withCollectionDeletionDisabled(
	provider tiltapiserver.StorageProvider,
	gvr schema.GroupVersionResource,
) tiltapiserver.StorageProvider {
	return func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		inner, providerErr := provider(scheme, getter)
		if providerErr != nil {
			return nil, fmt.Errorf("create storage for %s: %w", gvr, providerErr)
		}
		standard, standardOK := inner.(rest.StandardStorage)
		scoper, scoperOK := inner.(rest.Scoper)
		shortNames, shortNamesOK := inner.(rest.ShortNamesProvider)
		singularName, singularNameOK := inner.(rest.SingularNameProvider)
		if !standardOK || !scoperOK || !shortNamesOK || !singularNameOK {
			return nil, fmt.Errorf("storage for %s does not implement the filepath storage interfaces: %T", gvr, inner)
		}
		return &collectionDeletionDisabledStorage{
			StandardStorage:      standard,
			Scoper:               scoper,
			ShortNamesProvider:   shortNames,
			SingularNameProvider: singularName,
			gvr:                  gvr,
		}, nil
	}
}

type collectionDeletionDisabledStorage struct {
	rest.StandardStorage
	rest.Scoper
	rest.ShortNamesProvider
	rest.SingularNameProvider

	gvr schema.GroupVersionResource
}

func (storage *collectionDeletionDisabledStorage) DeleteCollection(
	context.Context,
	rest.ValidateObjectFunc,
	*metav1.DeleteOptions,
	*metainternalversion.ListOptions,
) (runtime.Object, error) {
	return nil, apierrors.NewMethodNotSupported(storage.gvr.GroupResource(), "deletecollection")
}

var _ rest.StandardStorage = (*collectionDeletionDisabledStorage)(nil)
var _ rest.Scoper = (*collectionDeletionDisabledStorage)(nil)
var _ rest.ShortNamesProvider = (*collectionDeletionDisabledStorage)(nil)
var _ rest.SingularNameProvider = (*collectionDeletionDisabledStorage)(nil)
