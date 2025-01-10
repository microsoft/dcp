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
					Enum:        []interface{}{"Running", "Stopping", "Stopped"},
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

type ApiServerExecutionStatus string

const (
	ApiServerRunning  ApiServerExecutionStatus = "Running"
	ApiServerStopping ApiServerExecutionStatus = "Stopping"
	ApiServerStopped  ApiServerExecutionStatus = "Stopped"
)

type ApiServerShutdownResourceCleanup string

const (
	ApiServerResourceCleanupNone ApiServerShutdownResourceCleanup = "None"
	ApiServerResourceCleanupFull ApiServerShutdownResourceCleanup = "Full"
)

type ApiServerExecutionData struct {
	Status                  ApiServerExecutionStatus         `json:"status"`
	ShutdownResourceCleanup ApiServerShutdownResourceCleanup `json:"shutdownResourceCleanup,omitempty"`
}
