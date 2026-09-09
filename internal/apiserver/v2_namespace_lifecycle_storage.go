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
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func decorateV2NamespaceStorageProviders(config *tiltapiserver.Config, gate *v2NamespaceLifecycleGate) error {
	for _, resource := range apiv2.PersistentTypes {
		gvr := resource.GetGroupVersionResource()
		provider, found := config.ExtraConfig.APIs[gvr]
		if !found || provider == nil {
			return fmt.Errorf("missing V2 storage provider for %s", gvr)
		}
		config.ExtraConfig.APIs[gvr] = withV2NamespaceLifecycleStorage(provider, gvr, gate)
	}
	return nil
}

func withV2NamespaceLifecycleStorage(
	provider tiltapiserver.StorageProvider,
	gvr schema.GroupVersionResource,
	gate *v2NamespaceLifecycleGate,
) tiltapiserver.StorageProvider {
	return func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		inner, providerErr := provider(scheme, getter)
		if providerErr != nil {
			return nil, fmt.Errorf("create V2 storage for %s: %w", gvr, providerErr)
		}
		standard, standardOK := inner.(rest.StandardStorage)
		scoper, scoperOK := inner.(rest.Scoper)
		shortNames, shortNamesOK := inner.(rest.ShortNamesProvider)
		singularName, singularNameOK := inner.(rest.SingularNameProvider)
		if !standardOK || !scoperOK || !shortNamesOK || !singularNameOK {
			return nil, fmt.Errorf("V2 storage for %s does not implement the filepath storage interfaces: %T", gvr, inner)
		}
		return &v2NamespaceLifecycleStorage{
			StandardStorage:      standard,
			Scoper:               scoper,
			ShortNamesProvider:   shortNames,
			SingularNameProvider: singularName,
			gate:                 gate,
			gvr:                  gvr,
		}, nil
	}
}

// v2NamespaceLifecycleStorage keeps lifecycle leases until storage returns, even if the HTTP
// request has already timed out. Request decoding and response delivery do not own leases.
type v2NamespaceLifecycleStorage struct {
	rest.StandardStorage
	rest.Scoper
	rest.ShortNamesProvider
	rest.SingularNameProvider

	gate *v2NamespaceLifecycleGate
	gvr  schema.GroupVersionResource
}

func (storage *v2NamespaceLifecycleStorage) Create(
	ctx context.Context,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	options *metav1.CreateOptions,
) (runtime.Object, error) {
	if options != nil && len(options.DryRun) != 0 {
		return nil, unsupportedV2DryRun()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if storage.gvr.Resource != "namespaces" {
		if storage.NamespaceScoped() {
			release, admissionErr := storage.beginChildCreate(ctx, false)
			if admissionErr != nil {
				return nil, admissionErr
			}
			defer release()
		}
		return storage.StandardStorage.Create(ctx, obj, createValidation, options)
	}

	metadata, metadataErr := meta.Accessor(obj)
	if metadataErr != nil {
		return nil, apierrors.NewInternalError(metadataErr)
	}
	namespace := metadata.GetName()
	mutationLease, mutationErr := storage.gate.beginNamespaceMutation(ctx, namespace)
	if mutationErr != nil {
		return nil, mutationErr
	}
	defer mutationLease.complete()

	outcome := v2NamespaceMutationUncertain
	defer func() { storage.completeNamespaceCreate(namespace, outcome) }()
	created, createErr := storage.StandardStorage.Create(ctx, obj, createValidation, options)
	outcome = v2NamespaceMutationAccepted
	if createErr != nil {
		// Tilt Create and Delete return errors only before persistence succeeds.
		outcome = v2NamespaceMutationRejected
	}
	return created, createErr
}

func (storage *v2NamespaceLifecycleStorage) Delete(
	ctx context.Context,
	name string,
	deleteValidation rest.ValidateObjectFunc,
	options *metav1.DeleteOptions,
) (runtime.Object, bool, error) {
	if options != nil && len(options.DryRun) != 0 {
		return nil, false, unsupportedV2DryRun()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, false, contextErr
	}
	if storage.gvr.Resource != "namespaces" {
		return storage.StandardStorage.Delete(ctx, name, deleteValidation, options)
	}

	mutationLease, mutationErr := storage.gate.beginNamespaceMutation(ctx, name)
	if mutationErr != nil {
		return nil, false, mutationErr
	}
	defer mutationLease.complete()
	deleteLease, deleteWaitErr := storage.gate.beginDelete(ctx, name)
	if deleteWaitErr != nil {
		return nil, false, deleteWaitErr
	}
	outcome := v2NamespaceMutationUncertain
	defer func() { deleteLease.complete(outcome) }()
	if contextErr := ctx.Err(); contextErr != nil {
		outcome = v2NamespaceMutationRejected
		return nil, false, contextErr
	}

	deleted, immediately, deleteErr := storage.StandardStorage.Delete(ctx, name, deleteValidation, options)
	outcome = v2NamespaceMutationAccepted
	if deleteErr != nil {
		outcome = v2NamespaceMutationRejected
	}
	return deleted, immediately, deleteErr
}

func (storage *v2NamespaceLifecycleStorage) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	if options != nil && len(options.DryRun) != 0 {
		return nil, false, unsupportedV2DryRun()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, false, contextErr
	}
	if !forceAllowCreate {
		return storage.StandardStorage.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	}
	if storage.gvr.Resource != "namespaces" {
		if storage.NamespaceScoped() {
			release, admissionErr := storage.beginChildCreate(ctx, true)
			if admissionErr != nil {
				return nil, false, admissionErr
			}
			defer release()
		}
		return storage.StandardStorage.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	}

	mutationLease, mutationErr := storage.gate.beginNamespaceMutation(ctx, name)
	if mutationErr != nil {
		return nil, false, mutationErr
	}
	defer mutationLease.complete()

	outcome := v2NamespaceMutationUncertain
	defer func() { storage.completeNamespaceCreate(name, outcome) }()
	updated, created, updateErr := storage.StandardStorage.Update(
		ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options,
	)
	if updateErr == nil {
		outcome = v2NamespaceMutationRejected
		if created {
			outcome = v2NamespaceMutationAccepted
		}
	}
	// Tilt Update can return an error after writing, while removing a finalized object.
	return updated, created, updateErr
}

func (storage *v2NamespaceLifecycleStorage) DeleteCollection(
	ctx context.Context,
	deleteValidation rest.ValidateObjectFunc,
	options *metav1.DeleteOptions,
	listOptions *metainternalversion.ListOptions,
) (runtime.Object, error) {
	if storage.gvr.Resource == "namespaces" {
		return nil, apierrors.NewMethodNotSupported(storage.gvr.GroupResource(), "deletecollection")
	}
	if options != nil && len(options.DryRun) != 0 {
		return nil, unsupportedV2DryRun()
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	return storage.StandardStorage.DeleteCollection(ctx, deleteValidation, options, listOptions)
}

func (storage *v2NamespaceLifecycleStorage) beginChildCreate(ctx context.Context, apply bool) (func(), error) {
	namespace, found := requestinfo.NamespaceFrom(ctx)
	if !found || namespace == "" {
		return nil, apierrors.NewBadRequest("missing namespace for V2 resource creation")
	}
	release, allowed := storage.gate.beginCreate(namespace)
	if !allowed {
		rejection := fmt.Errorf("cannot create resources in terminating namespace %q", namespace)
		if apply {
			rejection = fmt.Errorf(
				"cannot use server-side apply in terminating namespace %q because apply may create a missing resource; use update or a non-apply patch to modify an existing resource",
				namespace,
			)
		}
		return nil, apierrors.NewForbidden(storage.gvr.GroupResource(), "", rejection)
	}
	return release, nil
}

func (storage *v2NamespaceLifecycleStorage) completeNamespaceCreate(namespace string, outcome v2NamespaceMutationOutcome) {
	switch outcome {
	case v2NamespaceMutationAccepted:
		storage.gate.open(namespace)
	case v2NamespaceMutationUncertain:
		storage.gate.markCreateUncertain(namespace)
	}
}

func unsupportedV2DryRun() error {
	return apierrors.NewBadRequest("dry-run is not supported for top-level V2 resource mutations")
}

var _ rest.Storage = (*v2NamespaceLifecycleStorage)(nil)
var _ rest.StandardStorage = (*v2NamespaceLifecycleStorage)(nil)
var _ rest.Scoper = (*v2NamespaceLifecycleStorage)(nil)
var _ rest.ShortNamesProvider = (*v2NamespaceLifecycleStorage)(nil)
var _ rest.SingularNameProvider = (*v2NamespaceLifecycleStorage)(nil)
