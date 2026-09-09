/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"bytes"
	"context"
	"errors"
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
	activeCreates      int
	activeDeletes      int
	closed             bool
	deleteAccepted     bool
	deletionObserved   bool
	uncertainMutation  v2NamespaceMutationKind
	uncertaintyVersion uint64
	drained            chan struct{}
}

type v2NamespaceLifecycleGate struct {
	lock               sync.Mutex
	namespaces         map[string]*v2NamespaceLifecycleState
	namespaceMutations map[string]*v2NamespaceMutationState
	refreshRequested   chan struct{}
}

func newV2NamespaceLifecycleGate() *v2NamespaceLifecycleGate {
	return &v2NamespaceLifecycleGate{
		namespaces:         map[string]*v2NamespaceLifecycleState{},
		namespaceMutations: map[string]*v2NamespaceMutationState{},
		refreshRequested:   make(chan struct{}, 1),
	}
}

type v2NamespaceMutationKind uint8

const (
	v2NamespaceMutationNone v2NamespaceMutationKind = iota
	v2NamespaceMutationCreate
	v2NamespaceMutationDelete
)

type v2NamespaceMutationOutcome uint8

const (
	v2NamespaceMutationRejected v2NamespaceMutationOutcome = iota
	v2NamespaceMutationAccepted
	v2NamespaceMutationUncertain
)

type v2NamespaceMutationState struct {
	available  chan struct{}
	references int
}

type v2NamespaceMutationLease struct {
	gate      *v2NamespaceLifecycleGate
	namespace string
	state     *v2NamespaceMutationState
	once      sync.Once
}

func (gate *v2NamespaceLifecycleGate) beginNamespaceMutation(
	ctx context.Context,
	namespace string,
) (*v2NamespaceMutationLease, error) {
	gate.lock.Lock()
	state := gate.namespaceMutations[namespace]
	if state == nil {
		state = &v2NamespaceMutationState{available: make(chan struct{}, 1)}
		state.available <- struct{}{}
		gate.namespaceMutations[namespace] = state
	}
	state.references++
	gate.lock.Unlock()

	select {
	case <-state.available:
		if ctx.Err() != nil {
			state.available <- struct{}{}
			gate.releaseNamespaceMutationReference(namespace, state)
			return nil, ctx.Err()
		}
		return &v2NamespaceMutationLease{
			gate:      gate,
			namespace: namespace,
			state:     state,
		}, nil
	case <-ctx.Done():
		gate.releaseNamespaceMutationReference(namespace, state)
		return nil, ctx.Err()
	}
}

func (gate *v2NamespaceLifecycleGate) releaseNamespaceMutationReference(
	namespace string,
	state *v2NamespaceMutationState,
) {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	state.references--
	if state.references == 0 && gate.namespaceMutations[namespace] == state {
		delete(gate.namespaceMutations, namespace)
	}
}

func (lease *v2NamespaceMutationLease) complete() {
	lease.once.Do(func() {
		lease.state.available <- struct{}{}
		lease.gate.releaseNamespaceMutationReference(lease.namespace, lease.state)
	})
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
		lease.complete(v2NamespaceMutationRejected)
		return nil, ctx.Err()
	}
}

func (lease *v2NamespaceDeleteLease) complete(outcome v2NamespaceMutationOutcome) {
	lease.once.Do(func() {
		lease.gate.lock.Lock()

		refreshNeeded := false
		switch outcome {
		case v2NamespaceMutationAccepted:
			lease.state.deleteAccepted = true
			lease.state.closed = true
			lease.state.uncertainMutation = v2NamespaceMutationNone
		case v2NamespaceMutationUncertain:
			lease.state.closed = true
			lease.state.uncertainMutation = v2NamespaceMutationDelete
			lease.state.uncertaintyVersion++
			refreshNeeded = true
		}
		lease.state.activeDeletes--
		if lease.gate.removeObservedNamespaceDeletion(lease.namespace, lease.state) {
			lease.gate.lock.Unlock()
			return
		}
		if lease.state.activeDeletes == 0 && !lease.state.deleteAccepted &&
			lease.state.uncertainMutation == v2NamespaceMutationNone {
			lease.state.closed = false
		}
		if !lease.state.closed && lease.state.activeCreates == 0 &&
			lease.gate.namespaces[lease.namespace] == lease.state {
			delete(lease.gate.namespaces, lease.namespace)
		}
		lease.gate.lock.Unlock()

		if refreshNeeded {
			lease.gate.requestRefresh()
		}
	})
}

type v2NamespaceLifecycleSnapshot struct {
	state              *v2NamespaceLifecycleState
	uncertaintyVersion uint64
}

func (gate *v2NamespaceLifecycleGate) closedNamespaceStates() map[string]v2NamespaceLifecycleSnapshot {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	states := make(map[string]v2NamespaceLifecycleSnapshot)
	for namespace, state := range gate.namespaces {
		if state.closed {
			states[namespace] = v2NamespaceLifecycleSnapshot{
				state:              state,
				uncertaintyVersion: state.uncertaintyVersion,
			}
		}
	}
	return states
}

type v2NamespaceStorageState struct {
	terminating bool
}

func (gate *v2NamespaceLifecycleGate) observeNamespaces(
	namespaces map[string]v2NamespaceStorageState,
	closedStates map[string]v2NamespaceLifecycleSnapshot,
) {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	for namespace, snapshot := range closedStates {
		state := snapshot.state
		if gate.namespaces[namespace] != state || !state.closed {
			continue
		}
		storageState, found := namespaces[namespace]
		if !found {
			state.deletionObserved = true
			gate.removeObservedNamespaceDeletion(namespace, state)
			continue
		}
		if state.uncertainMutation == v2NamespaceMutationNone ||
			state.uncertaintyVersion != snapshot.uncertaintyVersion {
			continue
		}

		state.uncertainMutation = v2NamespaceMutationNone
		if storageState.terminating {
			state.deleteAccepted = true
			continue
		}

		state.closed = false
		state.deleteAccepted = false
		state.deletionObserved = false
		if state.activeCreates == 0 && state.activeDeletes == 0 {
			delete(gate.namespaces, namespace)
		}
	}
}

func (gate *v2NamespaceLifecycleGate) markCreateUncertain(namespace string) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state == nil || !state.closed {
		gate.lock.Unlock()
		return
	}
	state.uncertainMutation = v2NamespaceMutationCreate
	state.uncertaintyVersion++
	gate.lock.Unlock()

	gate.requestRefresh()
}

func (gate *v2NamespaceLifecycleGate) requestRefresh() {
	select {
	case gate.refreshRequested <- struct{}{}:
	default:
	}
}

// removeObservedNamespaceDeletion removes a closed lifecycle state after storage confirms that
// the Namespace is gone and all requests using the state have completed. The gate lock must be held.
func (gate *v2NamespaceLifecycleGate) removeObservedNamespaceDeletion(
	namespace string,
	state *v2NamespaceLifecycleState,
) bool {
	if !state.deletionObserved || state.activeCreates != 0 || state.activeDeletes != 0 ||
		gate.namespaces[namespace] != state {
		return false
	}
	delete(gate.namespaces, namespace)
	return true
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
	state.deletionObserved = false
	state.uncertainMutation = v2NamespaceMutationNone
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
	case handler.isNamespaceDeleteCollection(info):
		handler.writeError(
			writer,
			request,
			apierrors.NewMethodNotSupported(schema.GroupResource{Group: info.APIGroup, Resource: info.Resource}, "deletecollection"),
		)
	case handler.isNamespaceDelete(info) && !isDryRun(request):
		handler.handleNamespaceDelete(writer, request, info)
	case handler.isNamespaceCreate(info) && !isDryRun(request):
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

	// Server-side apply can create a missing object. At this HTTP boundary storage has not
	// determined whether the request is a create or update, so gate both to prevent a new
	// child from racing Namespace cleanup.
	return isServerSideApply(request)
}

func isServerSideApply(request *http.Request) bool {
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

func (*v2NamespaceLifecycleHandler) isNamespaceDeleteCollection(info *requestinfo.RequestInfo) bool {
	return info.Verb == "deletecollection" && info.Resource == "namespaces"
}

func isDryRun(request *http.Request) bool {
	return request.URL.Query().Has("dryRun")
}

func (handler *v2NamespaceLifecycleHandler) handleNamespacedResourceCreate(
	writer http.ResponseWriter,
	request *http.Request,
	info *requestinfo.RequestInfo,
) {
	release, allowed := handler.gate.beginCreate(info.Namespace)
	if !allowed {
		rejectionMessage := fmt.Sprintf("cannot create resources in terminating namespace %q", info.Namespace)
		if isServerSideApply(request) {
			rejectionMessage = fmt.Sprintf(
				"cannot use server-side apply in terminating namespace %q because apply may create a missing resource; use update or a non-apply patch to modify an existing resource",
				info.Namespace,
			)
		}
		handler.writeError(
			writer,
			request,
			apierrors.NewForbidden(
				schema.GroupResource{Group: info.APIGroup, Resource: info.Resource},
				"",
				errors.New(rejectionMessage),
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
	mutationLease, mutationWaitErr := handler.gate.beginNamespaceMutation(request.Context(), info.Name)
	if mutationWaitErr != nil {
		handler.writeError(
			writer,
			request,
			apierrors.NewTimeoutError(fmt.Sprintf("timed out waiting to delete namespace %q", info.Name), 0),
		)
		return
	}
	defer mutationLease.complete()

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
		outcome := v2NamespaceMutationRejected
		if !completed {
			outcome = v2NamespaceMutationUncertain
		} else if responseSucceeded(responseMetrics.Code) {
			outcome = v2NamespaceMutationAccepted
		}
		deleteLease.complete(outcome)
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

	mutationLease, mutationWaitErr := handler.gate.beginNamespaceMutation(request.Context(), namespaceName)
	if mutationWaitErr != nil {
		handler.writeError(
			writer,
			request,
			apierrors.NewTimeoutError(fmt.Sprintf("timed out waiting to create namespace %q", namespaceName), 0),
		)
		return
	}
	defer mutationLease.complete()

	responseMetrics := httpsnoop.Metrics{}
	completed := false
	defer func() {
		if namespaceName == "" {
			return
		}
		if !completed {
			handler.gate.markCreateUncertain(namespaceName)
		} else if responseSucceeded(responseMetrics.Code) {
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
