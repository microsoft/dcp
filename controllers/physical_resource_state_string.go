/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import "fmt"

func (progress physicalResourceProgress) String() string {
	switch progress {
	case 0:
		return "Unset"
	case physicalResourceProgressInProgress:
		return "InProgress"
	case physicalResourceProgressCompleted:
		return "Completed"
	case physicalResourceProgressRetryPending:
		return "RetryPending"
	case physicalResourceProgressFailed:
		return "Failed"
	case physicalResourceProgressNotFound:
		return "NotFound"
	case physicalResourceProgressNotReady:
		return "NotReady"
	case physicalResourceProgressTerminating:
		return "Terminating"
	case physicalResourceProgressNotActive:
		return "NotActive"
	case physicalResourceProgressRunning:
		return "Running"
	case physicalResourceProgressExited:
		return "Exited"
	case physicalResourceProgressDead:
		return "Dead"
	case physicalResourceProgressPaused:
		return "Paused"
	case physicalResourceProgressRestarting:
		return "Restarting"
	case physicalResourceProgressCreated:
		return "Created"
	case physicalResourceProgressRemoving:
		return "Removing"
	case physicalResourceProgressMissing:
		return "Missing"
	case physicalResourceProgressUnknown:
		return "Unknown"
	case physicalResourceProgressAbandoned:
		return "Abandoned"
	case physicalResourceProgressSkipped:
		return "Skipped"
	case physicalResourceProgressResultMissing:
		return "ResultMissing"
	default:
		return fmt.Sprintf("physicalResourceProgress(%d)", int(progress))
	}
}

func (state physicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerStateNamespace:
		return "Namespace"
	case physicalContainerStateResolve:
		return "Resolve"
	case physicalContainerStateImage:
		return "Image"
	case physicalContainerStateCreate:
		return "Create"
	case physicalContainerStateReplace:
		return "Replace"
	case physicalContainerStateCopyFiles:
		return "CopyFiles"
	case physicalContainerStateStart:
		return "Start"
	case physicalContainerStateCleanup:
		return "Cleanup"
	case physicalContainerStateRuntime:
		return "Runtime"
	case physicalContainerStateStop:
		return "Stop"
	case physicalContainerStateRemove:
		return "Remove"
	case physicalContainerStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalContainerState(%d)", int(state))
	}
}

func (state physicalContainerImageState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerImageStateNamespace:
		return "Namespace"
	case physicalContainerImageStateResolve:
		return "Resolve"
	case physicalContainerImageStatePull:
		return "Pull"
	case physicalContainerImageStateBuild:
		return "Build"
	case physicalContainerImageStateRuntime:
		return "Runtime"
	case physicalContainerImageStateDelete:
		return "Delete"
	case physicalContainerImageStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalContainerImageState(%d)", int(state))
	}
}

func (state physicalContainerNetworkState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerNetworkStateNamespace:
		return "Namespace"
	case physicalContainerNetworkStateResolve:
		return "Resolve"
	case physicalContainerNetworkStateCreate:
		return "Create"
	case physicalContainerNetworkStateReplace:
		return "Replace"
	case physicalContainerNetworkStateRuntime:
		return "Runtime"
	case physicalContainerNetworkStateRemove:
		return "Remove"
	case physicalContainerNetworkStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalContainerNetworkState(%d)", int(state))
	}
}

func (state physicalContainerVolumeState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerVolumeStateNamespace:
		return "Namespace"
	case physicalContainerVolumeStateResolve:
		return "Resolve"
	case physicalContainerVolumeStateCreate:
		return "Create"
	case physicalContainerVolumeStateReplace:
		return "Replace"
	case physicalContainerVolumeStateRuntime:
		return "Runtime"
	case physicalContainerVolumeStateRemove:
		return "Remove"
	case physicalContainerVolumeStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalContainerVolumeState(%d)", int(state))
	}
}

func (state physicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalProcessStateNamespace:
		return "Namespace"
	case physicalProcessStateResolve:
		return "Resolve"
	case physicalProcessStateLaunch:
		return "Launch"
	case physicalProcessStateRuntime:
		return "Runtime"
	case physicalProcessStateStop:
		return "Stop"
	case physicalProcessStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("physicalProcessState(%d)", int(state))
	}
}
