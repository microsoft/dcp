/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import "fmt"

func (state physicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerStateCleanupFailed:
		return "CleanupFailed"
	case physicalContainerStateCleanupRetry:
		return "CleanupRetry"
	case physicalContainerStateCopyFiles:
		return "CopyFiles"
	case physicalContainerStateCreate:
		return "Create"
	case physicalContainerStateCreateFailed:
		return "CreateFailed"
	case physicalContainerStateCreateRetry:
		return "CreateRetry"
	case physicalContainerStateCreated:
		return "Created"
	case physicalContainerStateCreationSkipped:
		return "CreationSkipped"
	case physicalContainerStateDead:
		return "Dead"
	case physicalContainerStateExited:
		return "Exited"
	case physicalContainerStateFileCopyFailed:
		return "FileCopyFailed"
	case physicalContainerStateFilesCopied:
		return "FilesCopied"
	case physicalContainerStateImage:
		return "Image"
	case physicalContainerStateImageFailed:
		return "ImageFailed"
	case physicalContainerStateImageLookupFailed:
		return "ImageLookupFailed"
	case physicalContainerStateImageNotFound:
		return "ImageNotFound"
	case physicalContainerStateInspectFailed:
		return "InspectFailed"
	case physicalContainerStateInvalid:
		return "Invalid"
	case physicalContainerStateMissing:
		return "Missing"
	case physicalContainerStateNetworkLookupFailed:
		return "NetworkLookupFailed"
	case physicalContainerStateNetworkNotFound:
		return "NetworkNotFound"
	case physicalContainerStateNetworks:
		return "Networks"
	case physicalContainerStateNotStarted:
		return "NotStarted"
	case physicalContainerStatePaused:
		return "Paused"
	case physicalContainerStateRemoveRetry:
		return "RemoveRetry"
	case physicalContainerStateRemoving:
		return "Removing"
	case physicalContainerStateReplaceRetry:
		return "ReplaceRetry"
	case physicalContainerStateResolve:
		return "Resolve"
	case physicalContainerStateResolveRetry:
		return "ResolveRetry"
	case physicalContainerStateRestarting:
		return "Restarting"
	case physicalContainerStateRunning:
		return "Running"
	case physicalContainerStateStart:
		return "Start"
	case physicalContainerStateStartFailed:
		return "StartFailed"
	case physicalContainerStateStarted:
		return "Started"
	case physicalContainerStateStopRetry:
		return "StopRetry"
	case physicalContainerStateStopped:
		return "Stopped"
	case physicalContainerStateUnknown:
		return "Unknown"
	case physicalContainerStateVolumeLookupFailed:
		return "VolumeLookupFailed"
	case physicalContainerStateVolumeNotFound:
		return "VolumeNotFound"
	case physicalContainerStateVolumes:
		return "Volumes"
	default:
		return fmt.Sprintf("physicalContainerState(%d)", int(state))
	}
}

func (state physicalContainerImageState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerImageStateBuild:
		return "Build"
	case physicalContainerImageStateBuildFailed:
		return "BuildFailed"
	case physicalContainerImageStateBuildResultMissing:
		return "BuildResultMissing"
	case physicalContainerImageStateBuilt:
		return "Built"
	case physicalContainerImageStateInspectFailed:
		return "InspectFailed"
	case physicalContainerImageStateInvalid:
		return "Invalid"
	case physicalContainerImageStateMissing:
		return "Missing"
	case physicalContainerImageStatePull:
		return "Pull"
	case physicalContainerImageStatePullFailed:
		return "PullFailed"
	case physicalContainerImageStatePullRetry:
		return "PullRetry"
	case physicalContainerImageStatePulled:
		return "Pulled"
	case physicalContainerImageStateReady:
		return "Ready"
	case physicalContainerImageStateResolve:
		return "Resolve"
	case physicalContainerImageStateUnavailable:
		return "Unavailable"
	default:
		return fmt.Sprintf("physicalContainerImageState(%d)", int(state))
	}
}

func (state physicalContainerNetworkState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerNetworkStateCreate:
		return "Create"
	case physicalContainerNetworkStateCreateFailed:
		return "CreateFailed"
	case physicalContainerNetworkStateCreateRetry:
		return "CreateRetry"
	case physicalContainerNetworkStateCreated:
		return "Created"
	case physicalContainerNetworkStateInspectFailed:
		return "InspectFailed"
	case physicalContainerNetworkStateInvalid:
		return "Invalid"
	case physicalContainerNetworkStateMissing:
		return "Missing"
	case physicalContainerNetworkStateReady:
		return "Ready"
	case physicalContainerNetworkStateRemove:
		return "Remove"
	case physicalContainerNetworkStateRemoveRetry:
		return "RemoveRetry"
	case physicalContainerNetworkStateRemoved:
		return "Removed"
	case physicalContainerNetworkStateReplaceRetry:
		return "ReplaceRetry"
	case physicalContainerNetworkStateReplaced:
		return "Replaced"
	case physicalContainerNetworkStateReplacementRejected:
		return "ReplacementRejected"
	case physicalContainerNetworkStateResolve:
		return "Resolve"
	default:
		return fmt.Sprintf("physicalContainerNetworkState(%d)", int(state))
	}
}

func (state physicalContainerVolumeState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalContainerVolumeStateCreate:
		return "Create"
	case physicalContainerVolumeStateCreateFailed:
		return "CreateFailed"
	case physicalContainerVolumeStateCreateRetry:
		return "CreateRetry"
	case physicalContainerVolumeStateCreated:
		return "Created"
	case physicalContainerVolumeStateInspectFailed:
		return "InspectFailed"
	case physicalContainerVolumeStateInvalid:
		return "Invalid"
	case physicalContainerVolumeStateMissing:
		return "Missing"
	case physicalContainerVolumeStateReady:
		return "Ready"
	case physicalContainerVolumeStateRemovalAbandoned:
		return "RemovalAbandoned"
	case physicalContainerVolumeStateRemove:
		return "Remove"
	case physicalContainerVolumeStateRemoveRetry:
		return "RemoveRetry"
	case physicalContainerVolumeStateRemoved:
		return "Removed"
	case physicalContainerVolumeStateReplaceRetry:
		return "ReplaceRetry"
	case physicalContainerVolumeStateReplaced:
		return "Replaced"
	case physicalContainerVolumeStateResolve:
		return "Resolve"
	default:
		return fmt.Sprintf("physicalContainerVolumeState(%d)", int(state))
	}
}

func (state physicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case physicalProcessStateExited:
		return "Exited"
	case physicalProcessStateInspectFailed:
		return "InspectFailed"
	case physicalProcessStateInvalid:
		return "Invalid"
	case physicalProcessStateLaunch:
		return "Launch"
	case physicalProcessStateLaunchFailed:
		return "LaunchFailed"
	case physicalProcessStateLaunchRetry:
		return "LaunchRetry"
	case physicalProcessStateLaunchSkipped:
		return "LaunchSkipped"
	case physicalProcessStateMissing:
		return "Missing"
	case physicalProcessStateResolve:
		return "Resolve"
	case physicalProcessStateResolveFailed:
		return "ResolveFailed"
	case physicalProcessStateResolveRetry:
		return "ResolveRetry"
	case physicalProcessStateRunning:
		return "Running"
	case physicalProcessStateStop:
		return "Stop"
	case physicalProcessStateStopRetry:
		return "StopRetry"
	default:
		return fmt.Sprintf("physicalProcessState(%d)", int(state))
	}
}

func (state logicalContainerState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalContainerStatePending:
		return "Pending"
	case logicalContainerStatePreparationFailed:
		return "PreparationFailed"
	case logicalContainerStateDelete:
		return "Delete"
	case logicalContainerStateExited:
		return "Exited"
	case logicalContainerStateFailed:
		return "Failed"
	case logicalContainerStateInvalid:
		return "Invalid"
	case logicalContainerStatePaused:
		return "Paused"
	case logicalContainerStateRunning:
		return "Running"
	case logicalContainerStateStopFailed:
		return "StopFailed"
	case logicalContainerStateStopped:
		return "Stopped"
	case logicalContainerStateStopping:
		return "Stopping"
	case logicalContainerStateUnknown:
		return "Unknown"
	case logicalContainerStateCreating:
		return "Creating"
	default:
		return fmt.Sprintf("logicalContainerState(%d)", int(state))
	}
}
