/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"net/http"
	"sync"

	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/microsoft/dcp/pkg/syncmap"
)

type resourceMutationKey struct {
	apiGroup string
	resource string
}

type orderedResourceMutationHandler struct {
	inner http.Handler
	locks *syncmap.Map[resourceMutationKey, sync.Locker]
}

func withOrderedResourceMutations(handler http.Handler) http.Handler {
	return &orderedResourceMutationHandler{
		inner: handler,
		locks: &syncmap.Map[resourceMutationKey, sync.Locker]{},
	}
}

func (h *orderedResourceMutationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestInfo, found := genericapirequest.RequestInfoFrom(r.Context())
	if !found || !requestInfo.IsResourceRequest || !isResourceMutation(requestInfo.Verb) {
		h.inner.ServeHTTP(w, r)
		return
	}

	// Tilt's storage shares one watch stream across versions and subresources of a GroupResource.
	// Serialize the complete mutation, including watcher notification, on the same boundary.
	key := resourceMutationKey{
		apiGroup: requestInfo.APIGroup,
		resource: requestInfo.Resource,
	}
	mutationLock, _ := h.locks.LoadOrStoreNew(key, func() sync.Locker {
		return &sync.Mutex{}
	})

	mutationLock.Lock()
	defer mutationLock.Unlock()
	h.inner.ServeHTTP(w, r)
}

func isResourceMutation(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	default:
		return false
	}
}
