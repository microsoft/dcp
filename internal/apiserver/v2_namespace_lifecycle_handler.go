/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type v2NamespaceLifecycleState struct {
	activeCreates  int
	activeDeletes  int
	closed         bool
	deleteAccepted bool
	drained        chan struct{}
}

type v2NamespaceLifecycleGate struct {
	lock       sync.Mutex
	namespaces map[string]*v2NamespaceLifecycleState
}

func newV2NamespaceLifecycleGate() *v2NamespaceLifecycleGate {
	return &v2NamespaceLifecycleGate{
		namespaces: map[string]*v2NamespaceLifecycleState{},
	}
}

func (gate *v2NamespaceLifecycleGate) beginCreate(namespace string) (func(), bool) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state != nil && state.closed {
		gate.lock.Unlock()
		return nil, false
	}
	if state == nil {
		state = &v2NamespaceLifecycleState{}
		gate.namespaces[namespace] = state
	}
	state.activeCreates++
	gate.lock.Unlock()

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			gate.lock.Lock()
			defer gate.lock.Unlock()

			state.activeCreates--
			if state.activeCreates != 0 {
				return
			}
			if state.drained != nil {
				close(state.drained)
				state.drained = nil
			}
			if !state.closed && gate.namespaces[namespace] == state {
				delete(gate.namespaces, namespace)
			}
		})
	}, true
}

type v2NamespaceDeleteLease struct {
	gate      *v2NamespaceLifecycleGate
	namespace string
	state     *v2NamespaceLifecycleState
	once      sync.Once
}

func (gate *v2NamespaceLifecycleGate) beginDelete(ctx context.Context, namespace string) (*v2NamespaceDeleteLease, error) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state == nil {
		state = &v2NamespaceLifecycleState{}
		gate.namespaces[namespace] = state
	}
	state.activeDeletes++
	state.closed = true
	lease := &v2NamespaceDeleteLease{
		gate:      gate,
		namespace: namespace,
		state:     state,
	}
	if state.activeCreates == 0 {
		gate.lock.Unlock()
		return lease, nil
	}
	if state.drained == nil {
		state.drained = make(chan struct{})
	}
	drained := state.drained
	gate.lock.Unlock()

	select {
	case <-drained:
		return lease, nil
	case <-ctx.Done():
		lease.complete(false)
		return nil, ctx.Err()
	}
}

func (lease *v2NamespaceDeleteLease) complete(accepted bool) {
	lease.once.Do(func() {
		lease.gate.lock.Lock()
		defer lease.gate.lock.Unlock()

		if accepted {
			lease.state.deleteAccepted = true
			lease.state.closed = true
		}
		lease.state.activeDeletes--
		if lease.state.activeDeletes == 0 && !lease.state.deleteAccepted {
			lease.state.closed = false
		}
		if !lease.state.closed && lease.state.activeCreates == 0 &&
			lease.gate.namespaces[lease.namespace] == lease.state {
			delete(lease.gate.namespaces, lease.namespace)
		}
	})
}

func (gate *v2NamespaceLifecycleGate) open(namespace string) {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	state := gate.namespaces[namespace]
	if state == nil {
		return
	}
	state.closed = false
	state.deleteAccepted = false
	if state.activeCreates == 0 && state.activeDeletes == 0 {
		delete(gate.namespaces, namespace)
	}
}

type v2NamespaceLifecycleHandler struct {
	inner        http.Handler
	gate         *v2NamespaceLifecycleGate
	resolver     requestinfo.RequestInfoResolver
	serializer   runtime.NegotiatedSerializer
	groupVersion schema.GroupVersion
	maxBodyBytes int64
}

func withV2NamespaceLifecycle(
	handler http.Handler,
	gate *v2NamespaceLifecycleGate,
	resolver requestinfo.RequestInfoResolver,
	serializer runtime.NegotiatedSerializer,
	maxBodyBytes int64,
) http.Handler {
	return &v2NamespaceLifecycleHandler{
		inner:        handler,
		gate:         gate,
		resolver:     resolver,
		serializer:   serializer,
		groupVersion: apiv2.GroupVersion,
		maxBodyBytes: maxBodyBytes,
	}
}

func (handler *v2NamespaceLifecycleHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	info, infoErr := handler.resolver.NewRequestInfo(request)
	if infoErr != nil || !handler.isV2Request(info) {
		handler.inner.ServeHTTP(writer, request)
		return
	}

	switch {
	case handler.isNamespacedResourceCreate(request, info):
		handler.handleNamespacedResourceCreate(writer, request, info)
	case handler.isDeleteCollection(info):
		handler.writeError(
			writer,
			request,
			apierrors.NewMethodNotSupported(schema.GroupResource{Group: info.APIGroup, Resource: info.Resource}, "deletecollection"),
		)
	case handler.isNamespaceDelete(info):
		handler.handleNamespaceDelete(writer, request, info)
	case handler.isNamespaceCreate(info):
		handler.handleNamespaceCreate(writer, request)
	default:
		handler.inner.ServeHTTP(writer, request)
	}
}

func (handler *v2NamespaceLifecycleHandler) isV2Request(info *requestinfo.RequestInfo) bool {
	return info.IsResourceRequest &&
		info.APIGroup == handler.groupVersion.Group &&
		info.APIVersion == handler.groupVersion.Version
}

func (*v2NamespaceLifecycleHandler) isNamespacedResourceCreate(
	request *http.Request,
	info *requestinfo.RequestInfo,
) bool {
	if info.Namespace == "" || info.Resource == "namespaces" || info.Subresource != "" {
		return false
	}
	if info.Verb == "create" {
		return true
	}
	if info.Verb != "patch" {
		return false
	}

	mediaType, _, mediaTypeErr := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaTypeErr != nil {
		return false
	}
	return mediaType == string(types.ApplyPatchType) || mediaType == string(types.ApplyCBORPatchType)
}

func (*v2NamespaceLifecycleHandler) isNamespaceCreate(info *requestinfo.RequestInfo) bool {
	return info.Verb == "create" && info.Resource == "namespaces" && info.Name == "" && info.Subresource == ""
}

func (*v2NamespaceLifecycleHandler) isNamespaceDelete(info *requestinfo.RequestInfo) bool {
	return info.Verb == "delete" && info.Resource == "namespaces" && info.Name != ""
}

func (*v2NamespaceLifecycleHandler) isDeleteCollection(info *requestinfo.RequestInfo) bool {
	return info.Verb == "deletecollection"
}

func (handler *v2NamespaceLifecycleHandler) handleNamespacedResourceCreate(
	writer http.ResponseWriter,
	request *http.Request,
	info *requestinfo.RequestInfo,
) {
	release, allowed := handler.gate.beginCreate(info.Namespace)
	if !allowed {
		handler.writeError(
			writer,
			request,
			apierrors.NewForbidden(
				schema.GroupResource{Group: info.APIGroup, Resource: info.Resource},
				"",
				fmt.Errorf("cannot create resources in terminating namespace %q", info.Namespace),
			),
		)
		return
	}
	defer release()

	handler.inner.ServeHTTP(writer, request)
}

func (handler *v2NamespaceLifecycleHandler) handleNamespaceDelete(
	writer http.ResponseWriter,
	request *http.Request,
	info *requestinfo.RequestInfo,
) {
	deleteLease, waitErr := handler.gate.beginDelete(request.Context(), info.Name)
	if waitErr != nil {
		handler.writeError(
			writer,
			request,
			apierrors.NewTimeoutError(fmt.Sprintf("timed out waiting for resource creation in namespace %q to finish", info.Name), 0),
		)
		return
	}

	responseMetrics := httpsnoop.Metrics{}
	completed := false
	defer func() {
		deleteLease.complete(!completed || responseSucceeded(responseMetrics.Code))
	}()
	responseMetrics.CaptureMetrics(writer, func(statusWriter http.ResponseWriter) {
		handler.inner.ServeHTTP(statusWriter, request)
	})
	completed = true
}

func (handler *v2NamespaceLifecycleHandler) handleNamespaceCreate(writer http.ResponseWriter, request *http.Request) {
	namespaceName, nameErr := handler.namespaceNameFromCreateRequest(request)
	if nameErr != nil {
		handler.writeError(writer, request, nameErr)
		return
	}

	responseMetrics := httpsnoop.Metrics{}
	completed := false
	defer func() {
		if namespaceName != "" && responseSucceeded(responseMetrics.Code) && (completed || responseMetrics.Code != 0) {
			handler.gate.open(namespaceName)
		}
	}()
	responseMetrics.CaptureMetrics(writer, func(statusWriter http.ResponseWriter) {
		handler.inner.ServeHTTP(statusWriter, request)
	})
	completed = true
}

func (handler *v2NamespaceLifecycleHandler) namespaceNameFromCreateRequest(request *http.Request) (string, error) {
	requestBody, readErr := handler.readRequestBody(request, "Namespace create")
	if readErr != nil {
		return "", readErr
	}
	serializerInfo, serializerErr := handler.serializerInfoForRequest(request, "Namespace create")
	if serializerErr != nil {
		return "", serializerErr
	}

	namespace := &apiv2.Namespace{}
	decoder := handler.serializer.DecoderToVersion(serializerInfo.Serializer, handler.groupVersion)
	if _, _, decodeErr := decoder.Decode(requestBody, nil, namespace); decodeErr != nil {
		return "", apierrors.NewBadRequest(fmt.Sprintf("failed to decode Namespace create request: %v", decodeErr))
	}
	return namespace.Name, nil
}

func (handler *v2NamespaceLifecycleHandler) readRequestBody(request *http.Request, operation string) ([]byte, error) {
	bodyReader := io.Reader(request.Body)
	if handler.maxBodyBytes > 0 {
		bodyReader = io.LimitReader(request.Body, handler.maxBodyBytes+1)
	}
	requestBody, readErr := io.ReadAll(bodyReader)
	if readErr != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("failed to read %s request: %v", operation, readErr))
	}
	closeErr := request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(requestBody))
	if closeErr != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("failed to close %s request body: %v", operation, closeErr))
	}
	if handler.maxBodyBytes > 0 && int64(len(requestBody)) > handler.maxBodyBytes {
		return nil, apierrors.NewRequestEntityTooLargeError(
			fmt.Sprintf("%s request body is too large: limit is %d bytes", operation, handler.maxBodyBytes),
		)
	}
	return requestBody, nil
}

func (handler *v2NamespaceLifecycleHandler) serializerInfoForRequest(
	request *http.Request,
	operation string,
) (runtime.SerializerInfo, error) {
	mediaType := runtime.ContentTypeJSON
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		parsedMediaType, _, parseErr := mime.ParseMediaType(contentType)
		if parseErr != nil {
			return runtime.SerializerInfo{}, apierrors.NewBadRequest(
				fmt.Sprintf("failed to parse %s request content type: %v", operation, parseErr),
			)
		}
		mediaType = parsedMediaType
	}
	serializerInfo, found := runtime.SerializerInfoForMediaType(handler.serializer.SupportedMediaTypes(), mediaType)
	if !found {
		return runtime.SerializerInfo{}, apierrors.NewBadRequest(
			fmt.Sprintf("unsupported %s request content type %q", operation, mediaType),
		)
	}
	return serializerInfo, nil
}

func (handler *v2NamespaceLifecycleHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	responsewriters.ErrorNegotiated(err, handler.serializer, handler.groupVersion, writer, request)
}

func responseSucceeded(statusCode int) bool {
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices
}
