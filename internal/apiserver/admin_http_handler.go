// Copyright (c) Microsoft Corporation. All rights reserved.

package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"

	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
)

const (
	AdminPathPrefix   = "/admin/"
	ExecutionDocument = "execution"
)

type adminHttpHandler struct {
	executionData   *ApiServerExecutionData
	requestShutdown func(ApiServerResourceCleanup)
	mux             *http.ServeMux
	log             logr.Logger
	lock            *sync.Mutex
}

func NewAdminHttpHandler(requestShutdown func(ApiServerResourceCleanup), log logr.Logger) http.Handler {
	if requestShutdown == nil {
		panic("requestShutdown must be provided")
	}

	mux := http.NewServeMux()
	ahh := &adminHttpHandler{
		executionData: &ApiServerExecutionData{
			Status:                  ApiServerRunning,
			ShutdownResourceCleanup: ApiServerResourceCleanupFull,
		},
		requestShutdown: requestShutdown,
		mux:             mux,
		log:             log.WithName("adminHttpHandler"),
		lock:            &sync.Mutex{},
	}

	mux.HandleFunc(
		fmt.Sprintf("GET %s%s", AdminPathPrefix, ExecutionDocument),
		func(w http.ResponseWriter, r *http.Request) { ahh.getExecutionData(w, r) },
	)
	mux.HandleFunc(
		fmt.Sprintf("PATCH %s%s", AdminPathPrefix, ExecutionDocument),
		func(w http.ResponseWriter, r *http.Request) { ahh.changeExecution(w, r) },
	)
	mux.Handle("/", http.NotFoundHandler())

	return ahh
}

func (h *adminHttpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *adminHttpHandler) getExecutionData(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && contentType != "application/json" {
		http.Error(w, "execution data is available in application/json format", http.StatusUnsupportedMediaType)
		return
	}

	var resp []byte
	var err error
	func() {
		h.lock.Lock()
		defer h.lock.Unlock()
		resp, err = json.Marshal(h.executionData)
	}()
	if err != nil {
		// Should never happen
		h.log.Error(err, "could not serialize API server execution data")
		http.Error(w, "could not serialize API server execution data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write(resp)
	if writeErr != nil {
		h.log.Error(writeErr, "could not write API server execution data")
	}
}

func (h *adminHttpHandler) changeExecution(w http.ResponseWriter, r *http.Request) {
	ctype := r.Header.Get("Content-Type")
	if ctype != "application/merge-patch+json" {
		http.Error(w, "execution patch must be in application/merge-patch+json format", http.StatusUnsupportedMediaType)
		return
	}

	reader := http.MaxBytesReader(w, r.Body, 512) // 512 bytes should be plenty for a ApiServerExecutionData patch
	body, bodyReadErr := io.ReadAll(reader)
	if bodyReadErr != nil {
		var tooLargeErr *http.MaxBytesError
		if errors.As(bodyReadErr, &tooLargeErr) {
			http.Error(w, "execution patch too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read execution patch request", http.StatusInternalServerError)
		}
		return
	}

	var patch ApiServerExecutionData
	unmarshalErr := json.Unmarshal(body, &patch)
	if unmarshalErr != nil {
		http.Error(w, "could not unmarshal execution patch request", http.StatusBadRequest)
		return
	}

	// In general a JSON merge patch document does not have to conform to the schema of the document it is patching.
	// (see https://datatracker.ietf.org/doc/html/rfc7396). But it the case of ApiServerExecutionData this is true.
	// If it ever becomes false, we will need a separate schema to validate the patch.
	validateErr := validate.AgainstSchema(&apiServerExecutionDataSpec, patch, strfmt.Default)
	if validateErr != nil {
		http.Error(w, "execution patch request does not conform to schema", http.StatusUnprocessableEntity)
		return
	}

	if patch.Status != ApiServerStopping && patch.Status != ApiServerCleaningResources {
		http.Error(w, "execution patch request can only set status to Stopping or CleaningResources", http.StatusUnprocessableEntity)
		return
	}

	h.lock.Lock()

	changingCleanupInProgressOrAfterCompleted := patch.ShutdownResourceCleanup != "" &&
		patch.ShutdownResourceCleanup != h.executionData.ShutdownResourceCleanup &&
		h.executionData.Status != ApiServerRunning
	if changingCleanupInProgressOrAfterCompleted {
		h.lock.Unlock()
		http.Error(w, "execution patch request cannot change resource cleanup type when cleanup is in progress or already done", http.StatusUnprocessableEntity)
		return
	}

	if h.executionData.Status == patch.Status {
		h.lock.Unlock()
		w.WriteHeader(http.StatusNoContent) // Nothing has changed, so we reply with NoContent.
		return
	}

	newStatus, found := validRequestStatusTransitions[apiServerStatusTransition{h.executionData.Status, patch.Status}]
	if !found {
		h.lock.Unlock()
		http.Error(w,
			fmt.Sprintf("the API server is in '%s' state and cannot transition to '%s' state", h.executionData.Status, patch.Status),
			http.StatusUnprocessableEntity,
		)
		return
	}

	oldStatus := h.executionData.Status
	changedStatus := oldStatus != newStatus
	h.executionData.Status = newStatus
	if changedStatus {
		h.log.Info("API server changed status", "OldStatus", oldStatus, "NewStatus", newStatus)
	}

	if patch.ShutdownResourceCleanup.IsFull() {
		h.executionData.ShutdownResourceCleanup = ApiServerResourceCleanupFull
	} else {
		h.executionData.ShutdownResourceCleanup = ApiServerResourceCleanupNone
	}

	if changedStatus && newStatus == ApiServerCleaningResources && h.executionData.ShutdownResourceCleanup.IsFull() {
		go func() {
			_ = appmgmt.CleanupAllResources(h.log)
			h.lock.Lock()
			defer h.lock.Unlock()

			// Update the status to CleanupComplete, but only if we haven't started the shutdown in the meantime.
			if h.executionData.Status == ApiServerCleaningResources {
				h.executionData.Status = ApiServerCleanupComplete
			}
		}()
	}

	resp, err := json.Marshal(h.executionData)
	h.lock.Unlock()
	if err != nil {
		// Should never happen
		h.log.Error(err, "could not serialize API server execution data")
		http.Error(w, "could not serialize API server execution data", http.StatusInternalServerError)
		return
	}

	if changedStatus {
		w.WriteHeader(http.StatusAccepted) // Accepted means operation has been started
	} else {
		w.WriteHeader(http.StatusOK) // OK means operation is in progress or has already been completed
	}
	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write(resp)
	if writeErr != nil {
		h.log.Error(writeErr, "could not write API server execution data")
	}

	// Only request shutdown AFTER writing the response, so that we do not "cancel ourselves" in the middle of writing.
	if changedStatus && newStatus == ApiServerStopping {
		h.requestShutdown(h.executionData.ShutdownResourceCleanup)
	}
}

var _ http.Handler = &adminHttpHandler{}
