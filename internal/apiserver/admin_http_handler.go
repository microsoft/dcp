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
)

const (
	AdminPathPrefix   = "/admin/"
	ExecutionDocument = "execution"
)

type adminHttpHandler struct {
	executionData   *ApiServerExecutionData
	requestShutdown func(ApiServerShutdownResourceCleanup)
	mux             *http.ServeMux
	log             logr.Logger
	lock            *sync.Mutex
}

func NewAdminHttpHandler(requestShutdown func(ApiServerShutdownResourceCleanup), log logr.Logger) http.Handler {
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

	var executionDataPatch ApiServerExecutionData
	unmarshalErr := json.Unmarshal(body, &executionDataPatch)
	if unmarshalErr != nil {
		http.Error(w, "could not unmarshal execution patch request", http.StatusBadRequest)
		return
	}

	// In general a JSON merge patch document does not have to conform to the schema of the document it is patching.
	// (see https://datatracker.ietf.org/doc/html/rfc7396). But it the case of ApiServerExecutionData this is true.
	// If it ever becomes false, we will need a separate schema to validate the patch.
	validateErr := validate.AgainstSchema(&apiServerExecutionDataSpec, executionDataPatch, strfmt.Default)
	if validateErr != nil {
		http.Error(w, "execution patch request does not conform to schema", http.StatusUnprocessableEntity)
		return
	}

	if executionDataPatch.Status != ApiServerStopping {
		http.Error(w, "execution patch request can only set status to stopping", http.StatusUnprocessableEntity)
		return
	}

	initiatedShutdown := false
	h.lock.Lock()

	changingResourceCleanupDuringShutdown := executionDataPatch.ShutdownResourceCleanup != "" &&
		executionDataPatch.ShutdownResourceCleanup != h.executionData.ShutdownResourceCleanup &&
		h.executionData.Status == ApiServerStopping
	if changingResourceCleanupDuringShutdown {
		h.lock.Unlock()
		http.Error(w, "execution patch request cannot change resource cleanup type when shutdown is in progress", http.StatusUnprocessableEntity)
		return
	}

	if h.executionData.Status == ApiServerRunning {
		h.executionData.Status = ApiServerStopping
		if executionDataPatch.ShutdownResourceCleanup != "" {
			h.executionData.ShutdownResourceCleanup = executionDataPatch.ShutdownResourceCleanup
		}
		h.requestShutdown(h.executionData.ShutdownResourceCleanup)
		initiatedShutdown = true
	}
	h.lock.Unlock()

	if initiatedShutdown {
		h.log.Info("API server shutdown initiated")
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

var _ http.Handler = &adminHttpHandler{}
