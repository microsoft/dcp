/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
	"os"
	stdfilepath "path/filepath"

	tiltresource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	builderrest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/rest"
	tiltfilepath "github.com/tilt-dev/tilt-apiserver/pkg/storage/filepath"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	registryrest "k8s.io/apiserver/pkg/registry/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/slices"
)

func newV2ResourceMemoryFS() tiltfilepath.FS {
	return tiltfilepath.NewMemoryFS()
}

func (s *ApiServer) withV2ResourceMemoryStorage(obj tiltresource.Object, rootPath string, fs tiltfilepath.FS) {
	watchSet := tiltfilepath.NewWatchSet()
	provider := newV2JSONFilepathStorageProvider(obj, rootPath, fs, watchSet, false)
	s.builder = s.builder.WithResourceAndHandler(obj, provider)

	if _, hasStatusSubresource := obj.(tiltresource.ObjectWithStatusSubResource); hasStatusSubresource {
		statusProvider := newV2JSONFilepathStorageProvider(obj, rootPath, fs, watchSet, true)
		s.builder = s.builder.WithSubResourceAndHandler(obj, "status", (&v2StatusProvider{provider: statusProvider}).Get)
	}
}

func newV2JSONFilepathStorageProvider(
	obj tiltresource.Object,
	rootPath string,
	fs tiltfilepath.FS,
	watchSet *tiltfilepath.WatchSet,
	statusSubresource bool,
) builderrest.ResourceHandlerProvider {
	return func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (registryrest.Storage, error) {
		gvr := obj.GetGroupVersionResource()
		groupResource := gvr.GroupResource()
		options, optionsErr := getter.GetRESTOptions(groupResource, obj)
		if optionsErr != nil {
			return nil, optionsErr
		}

		strategy := newV2ResourceStrategy(obj, scheme, options.StorageConfig.Codec, rootPath, fs)
		if statusSubresource {
			strategy = builderrest.StatusSubResourceStrategy{Strategy: strategy}
		}

		return tiltfilepath.NewFilepathREST(
			fs,
			watchSet,
			strategy,
			groupResource,
			options.StorageConfig.Codec,
			rootPath,
			obj.New,
			obj.NewList,
		), nil
	}
}

func newV2ResourceStrategy(
	obj tiltresource.Object,
	scheme *runtime.Scheme,
	codec runtime.Codec,
	rootPath string,
	fs tiltfilepath.FS,
) builderrest.Strategy {
	gvr := obj.GetGroupVersionResource()
	defaultStrategy := &builderrest.DefaultStrategy{
		Object:         obj,
		ObjectTyper:    scheme,
		TableConvertor: registryrest.NewDefaultTableConvertor(gvr.GroupResource()),
	}

	return &v2NamespaceLifecycleStrategy{
		DefaultStrategy: defaultStrategy,
		namespaceReader: &v2NamespaceLifecycleReader{
			fs:                fs,
			codec:             codec,
			namespaceRootPath: v2NamespaceRootPath(rootPath),
		},
	}
}

func v2NamespaceRootPath(rootPath string) string {
	return stdfilepath.Join(rootPath, apiv2.GroupVersion.Group, (&apiv2.Namespace{}).GetGroupVersionResource().Resource)
}

type v2NamespaceLifecycleStrategy struct {
	*builderrest.DefaultStrategy

	namespaceReader *v2NamespaceLifecycleReader
}

func (s *v2NamespaceLifecycleStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := s.DefaultStrategy.Validate(ctx, obj)
	if !v2ResourceRequiresNamespaceLifecycleCheck(obj) {
		return errorList
	}

	accessor, accessorErr := meta.Accessor(obj)
	if accessorErr != nil {
		return append(errorList, field.InternalError(field.NewPath("metadata"), accessorErr))
	}

	namespaceName, hasNamespace := genericapirequest.NamespaceFrom(ctx)
	if !hasNamespace {
		namespaceName = accessor.GetNamespace()
	}
	if namespaceName == "" {
		return errorList
	}

	validationMessage, validationErr := s.namespaceReader.validate(ctx, namespaceName)
	if validationErr != nil {
		return append(errorList, field.InternalError(field.NewPath("metadata", "namespace"), validationErr))
	}
	if validationMessage != "" {
		return append(errorList, field.Forbidden(field.NewPath("metadata", "namespace"), validationMessage))
	}

	return errorList
}

func v2ResourceRequiresNamespaceLifecycleCheck(obj runtime.Object) bool {
	switch obj.(type) {
	case *apiv2.PhysicalContainer, *apiv2.PhysicalContainerImage:
		return true
	default:
		return false
	}
}

type v2NamespaceLifecycleReader struct {
	fs                tiltfilepath.FS
	codec             runtime.Codec
	namespaceRootPath string
}

func (r *v2NamespaceLifecycleReader) validate(ctx context.Context, namespaceName string) (string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}

	namespace, getErr := r.get(namespaceName)
	if apierrors.IsNotFound(getErr) {
		return "namespace does not exist", nil
	}
	if getErr != nil {
		return "", getErr
	}

	if namespace.DeletionTimestamp != nil && !namespace.DeletionTimestamp.IsZero() {
		return "namespace is terminating", nil
	}
	if !slices.Contains(namespace.Finalizers, apiv2.NamespaceFinalizer) {
		return "namespace is not ready", nil
	}
	if namespace.Status.Phase != apiv2.NamespacePhaseActive {
		return "namespace is not active", nil
	}

	return "", nil
}

func (r *v2NamespaceLifecycleReader) get(namespaceName string) (*apiv2.Namespace, error) {
	namespacePath := stdfilepath.Join(r.namespaceRootPath, namespaceName+".json")
	obj, readErr := r.fs.Read(r.codec, namespacePath, func() runtime.Object { return &apiv2.Namespace{} })
	if os.IsNotExist(readErr) {
		return nil, apierrors.NewNotFound((&apiv2.Namespace{}).GetGroupVersionResource().GroupResource(), namespaceName)
	}
	if readErr != nil {
		return nil, fmt.Errorf("read namespace %q: %w", namespaceName, readErr)
	}

	namespace, ok := obj.(*apiv2.Namespace)
	if !ok {
		return nil, fmt.Errorf("read namespace %q: expected *v2.Namespace, got %T", namespaceName, obj)
	}
	return namespace, nil
}

type v2StatusProvider struct {
	provider builderrest.ResourceHandlerProvider
}

func (p *v2StatusProvider) Get(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (registryrest.Storage, error) {
	statusStorage, storageErr := p.provider(scheme, optsGetter)
	if storageErr != nil {
		return nil, storageErr
	}

	updater, ok := statusStorage.(registryrest.Updater)
	if !ok {
		return nil, fmt.Errorf("status storage does not support update: %T", statusStorage)
	}
	getter, ok := statusStorage.(registryrest.Getter)
	if !ok {
		return nil, fmt.Errorf("status storage does not support get: %T", statusStorage)
	}

	return &v2StatusStorage{
		Updater: updater,
		Getter:  getter,
	}, nil
}

type v2StatusStorage struct {
	registryrest.Updater
	registryrest.Getter
}

func (s *v2StatusStorage) Destroy() {
}

var _ builderrest.Strategy = (*v2NamespaceLifecycleStrategy)(nil)
