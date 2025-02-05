// Copyright (c) Microsoft Corporation. All rights reserved.

package apiserver

import (
	spec "k8s.io/kube-openapi/pkg/validation/spec"
)

var apiServerExecutionDataSpec = spec.Schema{
	SchemaProps: spec.SchemaProps{
		ID:          "github.com/microsoft/usvc-apiserver/api/v1.ApiServerExecutionData",
		Description: "Represents the API server execution state and allows for limited changes to that state.",
		Type:        []string{"object"},
		Properties: map[string]spec.Schema{
			"status": {
				SchemaProps: spec.SchemaProps{
					Description: "The current status of the API server.",
					Type:        []string{"string"},
					Enum:        []interface{}{"Running", "Stopping", "CleaningResources", "CleanupComplete"},
				},
			},
			"shutdownResourceCleanup": {
				SchemaProps: spec.SchemaProps{
					Description: "Type of resource cleanup performed by the API server on shutdown.",
					Type:        []string{"string"},
					Enum:        []interface{}{"None", "Full"},
					Default:     "Full",
				},
			},
		},
		Required: []string{"status"},
	},
}

// What status transitions are allowed in terms of what a PATCH request can do to the status field.
// The "key" is the current API server state.
// The "value" is a list of allowed states that the API server can transition to.
var validRequestStatusTransitions = map[ApiServerExecutionStatus][]ApiServerExecutionStatus{
	ApiServerRunning:           {ApiServerStopping, ApiServerCleaningResources},
	ApiServerCleaningResources: {ApiServerStopping, ApiServerCleaningResources},
	ApiServerStopping:          {ApiServerStopping},
	ApiServerCleanupComplete:   {ApiServerStopping},
}

type ApiServerExecutionStatus string

const (
	ApiServerRunning           ApiServerExecutionStatus = "Running"
	ApiServerCleaningResources ApiServerExecutionStatus = "CleaningResources"
	ApiServerStopping          ApiServerExecutionStatus = "Stopping"
	ApiServerCleanupComplete   ApiServerExecutionStatus = "CleanupComplete"
)

type ApiServerResourceCleanup string

const (
	// Do not perform any cleanup.
	ApiServerResourceCleanupNone ApiServerResourceCleanup = "None"

	// Perform full resource cleanup (default).
	ApiServerResourceCleanupFull ApiServerResourceCleanup = "Full"
)

func (rc ApiServerResourceCleanup) IsFull() bool {
	// Default is full cleanup, so treat it as such if the value is not set.
	return rc == ApiServerResourceCleanupFull || rc == ""
}

type ApiServerExecutionData struct {
	Status                  ApiServerExecutionStatus `json:"status"`
	ShutdownResourceCleanup ApiServerResourceCleanup `json:"shutdownResourceCleanup,omitempty"`
}
