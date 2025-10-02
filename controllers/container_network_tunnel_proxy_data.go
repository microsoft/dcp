// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"cmp"
	"os"
	std_slices "slices"
	"time"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type tunnelExtraData struct {
	// The number of preparation attempts made for this tunnel.
	// If the tunnel cannot be prepared after maximum number of attempts (currently 20)
	// it will be marked as failed.
	preparationAttempts uint32

	// Timestamp for the next preparation attempt (the next attempt must be made no earlier than this time).
	// This is necessary because when we make any changes to ContainerNetworkTunnelProxy,
	// they count as changes to the object and trigger another reconciliation loop shortly after the change.
	// We want to avoid making preparation attempts too frequently.
	nextPreparationNoEarlierThan time.Time

	// The names of client service Endpoints that have been created for this tunnel.
	clientServiceEndpointNames []types.NamespacedName
}

func (ted tunnelExtraData) Clone() tunnelExtraData {
	copy := tunnelExtraData{
		preparationAttempts:          ted.preparationAttempts,
		nextPreparationNoEarlierThan: ted.nextPreparationNoEarlierThan,
		clientServiceEndpointNames:   std_slices.Clone(ted.clientServiceEndpointNames),
	}
	return copy
}

func (ted tunnelExtraData) Equal(other tunnelExtraData) bool {
	if ted.preparationAttempts != other.preparationAttempts {
		return false
	}
	if !ted.nextPreparationNoEarlierThan.Equal(other.nextPreparationNoEarlierThan) {
		return false
	}
	if !std_slices.Equal(ted.clientServiceEndpointNames, other.clientServiceEndpointNames) {
		return false
	}
	return true
}

// Data we keep in memory for ContainerNetworkTunnelProxy instances.
type containerNetworkTunnelProxyData struct {
	apiv1.ContainerNetworkTunnelProxyStatus

	// Whether the startup of the proxy pair has been scheduled.
	// This is checked and updated when we enter the starting state.
	startupScheduled bool

	// Whether the cleanup of the proxy pair has been scheduled.
	// Graceful shutdown of the client proxy container and the server proxy process
	// can take a while, so we do it asynchronously.
	cleanupScheduled bool

	// Standard output file for the server proxy process.
	// Note: this is a file descriptor, and is not "cloned" when Clone() is called.
	serverStdout *os.File

	// Standard error file for the server proxy process.
	// Note: this is a file descriptor, and is not "cloned" when Clone() is called.
	serverStderr *os.File

	// Additional in-memory state for tunnels, keyed by tunnel name.
	tunnelExtra map[string]tunnelExtraData
}

func newContainerNetworkTunnelProxyData(state apiv1.ContainerNetworkTunnelProxyState) *containerNetworkTunnelProxyData {
	return &containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: apiv1.ContainerNetworkTunnelProxyStatus{
			State: state,
		},
		tunnelExtra: make(map[string]tunnelExtraData),
	}
}

func (tpd *containerNetworkTunnelProxyData) Clone() *containerNetworkTunnelProxyData {
	clone := containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: *tpd.ContainerNetworkTunnelProxyStatus.DeepCopy(),
		startupScheduled:                  tpd.startupScheduled,
		cleanupScheduled:                  tpd.cleanupScheduled,
		serverStdout:                      tpd.serverStdout,
		serverStderr:                      tpd.serverStderr,
		tunnelExtra:                       maps.Map[string, tunnelExtraData, tunnelExtraData](tpd.tunnelExtra, tunnelExtraData.Clone),
	}

	return &clone
}

func (tpd *containerNetworkTunnelProxyData) UpdateFrom(other *containerNetworkTunnelProxyData) bool {
	if other == nil {
		return false
	}

	updated := false

	if tpd.State != other.State {
		tpd.State = other.State
		updated = true
	}

	if !std_slices.EqualFunc(tpd.TunnelStatuses, other.TunnelStatuses, apiv1.TunnelStatus.Equal) {
		tpd.TunnelStatuses = slices.Map[apiv1.TunnelStatus, apiv1.TunnelStatus](other.TunnelStatuses, apiv1.TunnelStatus.Clone)
		updated = true
	}

	if tpd.TunnelConfigurationVersion != other.TunnelConfigurationVersion {
		tpd.TunnelConfigurationVersion = other.TunnelConfigurationVersion
		updated = true
	}

	if tpd.ClientProxyContainerImage != other.ClientProxyContainerImage {
		tpd.ClientProxyContainerImage = other.ClientProxyContainerImage
		updated = true
	}

	if tpd.ClientProxyContainerID != other.ClientProxyContainerID {
		tpd.ClientProxyContainerID = other.ClientProxyContainerID
		updated = true
	}

	if !pointers.EqualValue(tpd.ServerProxyProcessID, other.ServerProxyProcessID) {
		pointers.SetValueFrom(&tpd.ServerProxyProcessID, other.ServerProxyProcessID)
		updated = true
	}

	if tpd.ServerProxyStartupTimestamp != other.ServerProxyStartupTimestamp {
		tpd.ServerProxyStartupTimestamp = other.ServerProxyStartupTimestamp
		updated = true
	}

	if tpd.ServerProxyStdOutFile != other.ServerProxyStdOutFile {
		tpd.ServerProxyStdOutFile = other.ServerProxyStdOutFile
		updated = true
	}

	if tpd.ServerProxyStdErrFile != other.ServerProxyStdErrFile {
		tpd.ServerProxyStdErrFile = other.ServerProxyStdErrFile
		updated = true
	}

	if tpd.ClientProxyControlPort != other.ClientProxyControlPort {
		tpd.ClientProxyControlPort = other.ClientProxyControlPort
		updated = true
	}

	if tpd.ClientProxyDataPort != other.ClientProxyDataPort {
		tpd.ClientProxyDataPort = other.ClientProxyDataPort
		updated = true
	}

	if tpd.ServerProxyControlPort != other.ServerProxyControlPort {
		tpd.ServerProxyControlPort = other.ServerProxyControlPort
		updated = true
	}

	if tpd.startupScheduled != other.startupScheduled {
		tpd.startupScheduled = other.startupScheduled
		updated = true
	}

	if tpd.cleanupScheduled != other.cleanupScheduled {
		tpd.cleanupScheduled = other.cleanupScheduled
		updated = true
	}

	if other.serverStdout != nil && tpd.serverStdout != other.serverStdout {
		tpd.serverStdout = other.serverStdout
		updated = true
	}

	if other.serverStderr != nil && tpd.serverStderr != other.serverStderr {
		tpd.serverStderr = other.serverStderr
		updated = true
	}

	for k, v := range other.tunnelExtra {
		existing, found := tpd.tunnelExtra[k]
		if !found {
			tpd.tunnelExtra[k] = v.Clone()
			updated = true
		} else if !existing.Equal(v) {
			tpd.tunnelExtra[k] = v.Clone()
			updated = true
		}
	}

	return updated
}

func (tpd *containerNetworkTunnelProxyData) applyTo(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) objectChange {
	change := noChange

	if tpd.State != apiv1.ContainerNetworkTunnelProxyStateEmpty && tpd.State != tunnelProxy.Status.State {
		tunnelProxy.Status.State = tpd.State
		change |= statusChanged
	}

	// Do not check if len(tpd.TunnelStatuses) == 0 because we want to be able to clear the list of tunnel statuses.
	if !std_slices.EqualFunc(tpd.TunnelStatuses, tunnelProxy.Status.TunnelStatuses, apiv1.TunnelStatus.Equal) {
		tunnelProxy.Status.TunnelStatuses = slices.Map[apiv1.TunnelStatus, apiv1.TunnelStatus](tpd.TunnelStatuses, apiv1.TunnelStatus.Clone)
		change |= statusChanged
	}

	if tpd.TunnelConfigurationVersion != tunnelProxy.Status.TunnelConfigurationVersion {
		tunnelProxy.Status.TunnelConfigurationVersion = tpd.TunnelConfigurationVersion
		change |= statusChanged
	}

	if tpd.ClientProxyContainerImage != tunnelProxy.Status.ClientProxyContainerImage {
		tunnelProxy.Status.ClientProxyContainerImage = tpd.ClientProxyContainerImage
		change |= statusChanged
	}

	if tpd.ClientProxyContainerID != tunnelProxy.Status.ClientProxyContainerID {
		tunnelProxy.Status.ClientProxyContainerID = tpd.ClientProxyContainerID
		change |= statusChanged
	}

	if !pointers.EqualValue(tunnelProxy.Status.ServerProxyProcessID, tpd.ServerProxyProcessID) {
		pointers.SetValueFrom(&tunnelProxy.Status.ServerProxyProcessID, tpd.ServerProxyProcessID)
		change |= statusChanged
	}

	if tunnelProxy.Status.ServerProxyStartupTimestamp.IsZero() && !tpd.ServerProxyStartupTimestamp.IsZero() {
		tunnelProxy.Status.ServerProxyStartupTimestamp = tpd.ServerProxyStartupTimestamp
		change |= statusChanged
	}

	if tpd.ServerProxyStdOutFile != tunnelProxy.Status.ServerProxyStdOutFile {
		tunnelProxy.Status.ServerProxyStdOutFile = tpd.ServerProxyStdOutFile
		change |= statusChanged
	}

	if tpd.ServerProxyStdErrFile != tunnelProxy.Status.ServerProxyStdErrFile {
		tunnelProxy.Status.ServerProxyStdErrFile = tpd.ServerProxyStdErrFile
		change |= statusChanged
	}

	if tpd.ClientProxyControlPort != tunnelProxy.Status.ClientProxyControlPort {
		tunnelProxy.Status.ClientProxyControlPort = tpd.ClientProxyControlPort
		change |= statusChanged
	}

	if tpd.ClientProxyDataPort != tunnelProxy.Status.ClientProxyDataPort {
		tunnelProxy.Status.ClientProxyDataPort = tpd.ClientProxyDataPort
		change |= statusChanged
	}

	if tpd.ServerProxyControlPort != tunnelProxy.Status.ServerProxyControlPort {
		tunnelProxy.Status.ServerProxyControlPort = tpd.ServerProxyControlPort
		change |= statusChanged
	}

	return change
}

// Adds or updates a tunnel status in the proxy data.
// To make status comparison and change detection easy, tunnel status are always sorted by tunnel name.
func (tpd *containerNetworkTunnelProxyData) setTunnelStatus(ts apiv1.TunnelStatus) {
	i, found := std_slices.BinarySearchFunc(tpd.TunnelStatuses, ts.Name, func(current apiv1.TunnelStatus, target string) int {
		return cmp.Compare(current.Name, target)
	})
	if found {
		tpd.TunnelStatuses[i] = ts
	} else {
		tpd.TunnelStatuses = std_slices.Insert(tpd.TunnelStatuses, i, ts)
	}
}

// Removes a tunnels status from the proxy data.
// It is a no-op if the tunnel status does not exist.
func (tpd *containerNetworkTunnelProxyData) removeTunnelStatus(tunnelName string) {
	i, found := std_slices.BinarySearchFunc(tpd.TunnelStatuses, tunnelName, func(current apiv1.TunnelStatus, target string) int {
		return cmp.Compare(current.Name, target)
	})
	if found {
		// Remove existing status
		tpd.TunnelStatuses = std_slices.Delete(tpd.TunnelStatuses, i, i+1)
	}
}

// Returns all known tunnel statuses associated with given client service namespaced name.
func (tpd *containerNetworkTunnelProxyData) tunnelsForClientService(tunnels []apiv1.TunnelConfiguration, csName types.NamespacedName) []apiv1.TunnelStatus {
	tunnelNames := slices.Map[apiv1.TunnelConfiguration, string](tunnels, func(tc apiv1.TunnelConfiguration) string {
		nn := types.NamespacedName{
			Namespace: tc.ClientServiceNamespace,
			Name:      tc.ClientServiceName,
		}
		if nn == csName {
			return tc.Name
		}
		return ""
	})
	tunnelNames = slices.NonEmpty[string](tunnelNames)

	statuses := slices.Select(tpd.TunnelStatuses, func(ts apiv1.TunnelStatus) bool {
		return slices.Contains(tunnelNames, ts.Name)
	})
	return statuses
}

var _ Cloner[*containerNetworkTunnelProxyData] = (*containerNetworkTunnelProxyData)(nil)
var _ UpdateableFrom[*containerNetworkTunnelProxyData] = (*containerNetworkTunnelProxyData)(nil)
