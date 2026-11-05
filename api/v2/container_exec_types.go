/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// ContainerExecPhase describes a single attached command execution within a container.
type ContainerExecPhase string

const (
	// ContainerExecReasonContainerNotFound indicates that the execution target has not been found.
	ContainerExecReasonContainerNotFound ConditionReason = "ContainerNotFound"
	// ContainerExecReasonContainerNotReady indicates that the execution target is not running and ready.
	ContainerExecReasonContainerNotReady ConditionReason = "ContainerNotReady"
	// ContainerExecReasonContainerLookupFailed indicates that the execution target could not be read.
	ContainerExecReasonContainerLookupFailed ConditionReason = "ContainerLookupFailed"
	// ContainerExecReasonContainerUnavailable indicates that the selected target cannot be used for execution.
	ContainerExecReasonContainerUnavailable ConditionReason = "ContainerUnavailable"
	// ContainerExecReasonLaunching indicates that attached command launch is in progress.
	ContainerExecReasonLaunching ConditionReason = "Launching"
	// ContainerExecReasonLaunchQueueFailed indicates that attached command launch could not be queued.
	ContainerExecReasonLaunchQueueFailed ConditionReason = "LaunchQueueFailed"
	// ContainerExecReasonLaunchFailed indicates that attached command launch failed.
	ContainerExecReasonLaunchFailed ConditionReason = "LaunchFailed"
	// ContainerExecReasonCaptureFailed indicates that execution output capture could not be prepared.
	ContainerExecReasonCaptureFailed ConditionReason = "CaptureFailed"
	// ContainerExecReasonRunning indicates that the attached command is running.
	ContainerExecReasonRunning ConditionReason = "ExecutionRunning"
	// ContainerExecReasonExited indicates that the attached command has completed.
	ContainerExecReasonExited ConditionReason = "ExecutionExited"
	// ContainerExecReasonStopping indicates that execution cancellation is in progress.
	ContainerExecReasonStopping ConditionReason = "Stopping"
	// ContainerExecReasonCancelled indicates that execution was cancelled before launch.
	ContainerExecReasonCancelled ConditionReason = "ExecutionCancelled"
	// ContainerExecReasonCompletionUnknown indicates that execution completion was not reported.
	ContainerExecReasonCompletionUnknown ConditionReason = "CompletionUnknown"
)

const (
	ContainerExecPhasePending ContainerExecPhase = "Pending"
	ContainerExecPhaseRunning ContainerExecPhase = "Running"
	ContainerExecPhaseExited  ContainerExecPhase = "Exited"
	ContainerExecPhaseFailed  ContainerExecPhase = "Failed"
)

// ContainerExecConfig describes literal command inputs. No DCP template expansion is performed.
// +k8s:openapi-gen=true
type ContainerExecConfig struct {
	// Command is the executable name or path inside the container.
	Command string `json:"command"`
	// Args contains arguments passed to the command.
	// +listType=atomic
	Args []string `json:"args,omitempty"`
	// WorkingDirectory is the working directory inside the container.
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// Env overrides the container environment for this execution.
	// +listType=map
	// +listMapKey=name
	Env []commonapi.EnvVar `json:"env,omitempty"`
	// EnvFiles contains host file paths read by the container runtime to populate the environment.
	// +listType=set
	EnvFiles []string `json:"envFiles,omitempty"`
}

func (config *ContainerExecConfig) validate(path *field.Path) field.ErrorList {
	var errorList field.ErrorList
	if strings.TrimSpace(config.Command) == "" {
		errorList = append(errorList, field.Required(path.Child("command"), "command must be set"))
	}
	for i, env := range config.Env {
		if env.Name == "" {
			errorList = append(errorList, field.Required(path.Child("env").Index(i).Child("name"), "name must be set"))
		}
	}
	for i, envFile := range config.EnvFiles {
		if strings.TrimSpace(envFile) == "" {
			errorList = append(errorList, field.Required(path.Child("envFiles").Index(i), "file path must be set"))
		}
	}
	return errorList
}

// ContainerExecStatus describes an attached execution, not the host runtime CLI process.
// +k8s:openapi-gen=true
type ContainerExecStatus struct {
	// Phase summarizes the execution lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Exited;Failed
	Phase ContainerExecPhase `json:"phase,omitempty"`
	// StartedAt is when the runtime accepted the attached command launch.
	// It does not confirm that the command inside the container started successfully.
	StartedAt metav1.MicroTime `json:"startedAt,omitempty"`
	// FinishedAt is when attached execution completion or a terminal startup failure was observed.
	FinishedAt metav1.MicroTime `json:"finishedAt,omitempty"`
	// ExitCode is the exit code reported by the runtime command, when available.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Conditions describe prerequisite readiness and execution progress.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
