/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"time"

	"github.com/microsoft/dcp/internal/dap/proto"
	"github.com/microsoft/dcp/pkg/commonapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ToNamespacedNameWithKind converts a proto ResourceIdentifier to a commonapi.NamespacedNameWithKind.
func ToNamespacedNameWithKind(ri *proto.ResourceIdentifier) commonapi.NamespacedNameWithKind {
	if ri == nil {
		return commonapi.NamespacedNameWithKind{}
	}

	return commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: ri.GetNamespace(),
			Name:      ri.GetName(),
		},
		Kind: schema.GroupVersionKind{
			Group:   ri.GetGroup(),
			Version: ri.GetVersion(),
			Kind:    ri.GetKind(),
		},
	}
}

// FromNamespacedNameWithKind converts a commonapi.NamespacedNameWithKind to a proto ResourceIdentifier.
func FromNamespacedNameWithKind(nnk commonapi.NamespacedNameWithKind) *proto.ResourceIdentifier {
	return &proto.ResourceIdentifier{
		Namespace: ptrString(nnk.Namespace),
		Name:      ptrString(nnk.Name),
		Group:     ptrString(nnk.Kind.Group),
		Version:   ptrString(nnk.Kind.Version),
		Kind:      ptrString(nnk.Kind.Kind),
	}
}

// ptrString returns a pointer to the given string.
func ptrString(s string) *string {
	return &s
}

// ptrBool returns a pointer to the given bool.
func ptrBool(b bool) *bool {
	return &b
}

// ptrInt64 returns a pointer to the given int64.
func ptrInt64(i int64) *int64 {
	return &i
}

// ToDebugSessionStatus converts a proto DebugSessionStatus to a DebugSessionStatus.
func ToDebugSessionStatus(status proto.DebugSessionStatus) DebugSessionStatus {
	switch status {
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_CONNECTING:
		return DebugSessionStatusConnecting
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_INITIALIZING:
		return DebugSessionStatusInitializing
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_ATTACHED:
		return DebugSessionStatusAttached
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_STOPPED:
		return DebugSessionStatusStopped
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_TERMINATED:
		return DebugSessionStatusTerminated
	case proto.DebugSessionStatus_DEBUG_SESSION_STATUS_ERROR:
		return DebugSessionStatusError
	default:
		return DebugSessionStatusConnecting
	}
}

// ToDebugSessionStatusFromPtr converts a proto DebugSessionStatus pointer to a DebugSessionStatus.
func ToDebugSessionStatusFromPtr(status *proto.DebugSessionStatus) DebugSessionStatus {
	if status == nil {
		return DebugSessionStatusConnecting
	}
	return ToDebugSessionStatus(*status)
}

// FromDebugSessionStatus converts a DebugSessionStatus to a proto DebugSessionStatus pointer.
func FromDebugSessionStatus(status DebugSessionStatus) *proto.DebugSessionStatus {
	var ps proto.DebugSessionStatus
	switch status {
	case DebugSessionStatusConnecting:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_CONNECTING
	case DebugSessionStatusInitializing:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_INITIALIZING
	case DebugSessionStatusAttached:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_ATTACHED
	case DebugSessionStatusStopped:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_STOPPED
	case DebugSessionStatusTerminated:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_TERMINATED
	case DebugSessionStatusError:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_ERROR
	default:
		ps = proto.DebugSessionStatus_DEBUG_SESSION_STATUS_UNSPECIFIED
	}
	return &ps
}

// toProtoAdapterConfig converts a DebugAdapterConfig to a proto.DebugAdapterConfig.
func toProtoAdapterConfig(config *DebugAdapterConfig) *proto.DebugAdapterConfig {
	if config == nil {
		return nil
	}

	protoConfig := &proto.DebugAdapterConfig{
		Args: config.Args,
		Mode: toProtoAdapterMode(config.Mode),
	}

	// Convert environment variables
	if len(config.Env) > 0 {
		protoConfig.Env = make([]*proto.EnvVar, len(config.Env))
		for i, ev := range config.Env {
			protoConfig.Env[i] = &proto.EnvVar{
				Name:  ptrString(ev.Name),
				Value: ptrString(ev.Value),
			}
		}
	}

	// Convert connection timeout
	if config.ConnectionTimeout > 0 {
		protoConfig.ConnectionTimeoutSeconds = ptrInt32(int32(config.ConnectionTimeout.Seconds()))
	}

	return protoConfig
}

// toProtoAdapterMode converts a DebugAdapterMode to a proto.DebugAdapterMode pointer.
func toProtoAdapterMode(mode DebugAdapterMode) *proto.DebugAdapterMode {
	var pm proto.DebugAdapterMode
	switch mode {
	case DebugAdapterModeStdio:
		pm = proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_STDIO
	case DebugAdapterModeTCPCallback:
		pm = proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_TCP_CALLBACK
	case DebugAdapterModeTCPConnect:
		pm = proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_TCP_CONNECT
	default:
		pm = proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_UNSPECIFIED
	}
	return &pm
}

// FromProtoAdapterConfig converts a proto.DebugAdapterConfig to a DebugAdapterConfig.
func FromProtoAdapterConfig(config *proto.DebugAdapterConfig) *DebugAdapterConfig {
	if config == nil {
		return nil
	}

	result := &DebugAdapterConfig{
		Args: config.GetArgs(),
		Mode: fromProtoAdapterMode(config.GetMode()),
	}

	// Convert environment variables
	if len(config.GetEnv()) > 0 {
		result.Env = make([]EnvVar, len(config.GetEnv()))
		for i, ev := range config.GetEnv() {
			result.Env[i] = EnvVar{
				Name:  ev.GetName(),
				Value: ev.GetValue(),
			}
		}
	}

	// Convert connection timeout
	if config.GetConnectionTimeoutSeconds() > 0 {
		result.ConnectionTimeout = time.Duration(config.GetConnectionTimeoutSeconds()) * time.Second
	}

	return result
}

// fromProtoAdapterMode converts a proto.DebugAdapterMode to a DebugAdapterMode.
func fromProtoAdapterMode(mode proto.DebugAdapterMode) DebugAdapterMode {
	switch mode {
	case proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_STDIO:
		return DebugAdapterModeStdio
	case proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_TCP_CALLBACK:
		return DebugAdapterModeTCPCallback
	case proto.DebugAdapterMode_DEBUG_ADAPTER_MODE_TCP_CONNECT:
		return DebugAdapterModeTCPConnect
	default:
		return DebugAdapterModeStdio
	}
}

// ptrInt32 returns a pointer to the given int32.
func ptrInt32(i int32) *int32 {
	return &i
}
