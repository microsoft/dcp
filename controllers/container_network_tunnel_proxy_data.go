/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"cmp"
	"context"
	std_maps "maps"
	"os"
	std_slices "slices"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	stdproto "google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
)

// tunnelProxyRecoverySchedule retains retry timing across replacement pairs.
type tunnelProxyRecoverySchedule struct {
	// Earliest time at which the next replacement pair may be started.
	retryAfter time.Time

	// Per-proxy retry progression, advanced only by reconciliation.
	backoff backoff.BackOff

	// Consecutive pair generations whose server proxy did not establish its first control stream.
	// Client container failures during replacement startup are not counted.
	neverReadyGenerations uint32
}

type tunnelExtraData struct {
	// Consecutive failed PrepareTunnel attempts for a tunnel without a published listener port.
	// Unpublished tunnels transition to Failed after reaching MaxTunnelPreparationAttempts;
	// previously published tunnels retry rebinding indefinitely to preserve their URL.
	// Each proxy pair generation starts counting from zero.
	preparationAttempts uint32

	// Earliest timestamp at which the next tunnel control operation (PrepareTunnel or
	// suspend/resume UpdateTunnel) may run, pacing retries across watch-triggered reconciliations.
	nextPreparationNoEarlierThan time.Time

	// The names of client service Endpoints that have been created for this tunnel.
	clientServiceEndpointNames []types.NamespacedName

	// Name of the client Service whose Endpoints this tunnel produces, used for cache invalidation on deletion.
	clientServiceName string

	// The exact preparation request being retried until its runtime tunnel ID is observed.
	pendingPreparationRequest *dcptunproto.TunnelReq

	// The server endpoint currently configured in the proxy pair.
	serverAddress string
	serverPort    int32

	// Whether the proxy pair is currently accepting new streams for this tunnel.
	acceptingNewStreams bool

	// Whether acceptingNewStreams reflects a confirmed proxy-pair update.
	acceptanceStateKnown bool
}

func (ted tunnelExtraData) Clone() tunnelExtraData {
	copy := tunnelExtraData{
		preparationAttempts:          ted.preparationAttempts,
		nextPreparationNoEarlierThan: ted.nextPreparationNoEarlierThan,
		clientServiceEndpointNames:   std_slices.Clone(ted.clientServiceEndpointNames),
		clientServiceName:            ted.clientServiceName,
		pendingPreparationRequest:    stdproto.CloneOf(ted.pendingPreparationRequest),
		serverAddress:                ted.serverAddress,
		serverPort:                   ted.serverPort,
		acceptingNewStreams:          ted.acceptingNewStreams,
		acceptanceStateKnown:         ted.acceptanceStateKnown,
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
	if ted.clientServiceName != other.clientServiceName {
		return false
	}
	if !stdproto.Equal(ted.pendingPreparationRequest, other.pendingPreparationRequest) {
		return false
	}
	if ted.serverAddress != other.serverAddress ||
		ted.serverPort != other.serverPort ||
		ted.acceptingNewStreams != other.acceptingNewStreams ||
		ted.acceptanceStateKnown != other.acceptanceStateKnown {
		return false
	}
	return true
}

// startupLease serializes proxy-pair startup work across cloned in-memory state
// and allows deletion or shutdown to cancel an in-flight startup wait promptly.
type startupLease struct {
	lock       *concurrency.ContextAwareLock
	cancelLock sync.Mutex
	cancel     context.CancelFunc
}

func newStartupLease() *startupLease {
	return &startupLease{
		lock: concurrency.NewContextAwareLock(),
	}
}

func (lease *startupLease) TryAcquire(lifetimeCtx context.Context) (context.Context, func(), bool) {
	if lifetimeCtx.Err() != nil || !lease.lock.TryLock() {
		return nil, nil, false
	}

	attemptCtx, cancelAttempt := context.WithCancel(lifetimeCtx)
	lease.cancelLock.Lock()
	lease.cancel = cancelAttempt
	lease.cancelLock.Unlock()

	unlock := sync.OnceFunc(func() {
		cancelAttempt()
		lease.cancelLock.Lock()
		lease.cancel = nil
		lease.cancelLock.Unlock()
		lease.lock.Unlock()
	})
	stopUnlockOnShutdown := context.AfterFunc(lifetimeCtx, unlock)
	release := sync.OnceFunc(func() {
		stopUnlockOnShutdown()
		unlock()
	})

	if lifetimeCtx.Err() != nil {
		release()
		return nil, nil, false
	}

	return attemptCtx, release, true
}

// CancelActive cancels the context of the currently held startup attempt, if any,
// without unlocking the lease itself (the active attempt still owns lease release).
func (lease *startupLease) CancelActive() {
	lease.cancelLock.Lock()
	cancel := lease.cancel
	lease.cancelLock.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Data we keep in memory for ContainerNetworkTunnelProxy instances.
type containerNetworkTunnelProxyData struct {
	// Authoritative proxy lifecycle and status details.
	apiv1.ContainerNetworkTunnelProxyStatus

	// Identifies the logical proxy that owns this data.
	resourceUID types.UID

	// Identifies the pair incarnation for fencing and startup classification; zero denotes the initial pair.
	pairGeneration uint64

	// Identifies the client physical container this pair is allowed to clean up.
	clientPhysicalUID types.UID

	// Preserves retry progression and the next replacement time across pair generations.
	recovery tunnelProxyRecoverySchedule

	// Time at which this pair generation established its first control stream; zero until then.
	controlReadyAt time.Time

	// Preserves consumer-facing port allocations across pair replacements, keyed by tunnel name.
	publishedPorts map[string]int32

	// Serializes startup and cleanup against asynchronous startup results.
	startup *startupLease

	// Captures server proxy standard output; clones share the file descriptor.
	serverStdout *os.File

	// Captures server proxy standard error; clones share the file descriptor.
	serverStderr *os.File

	// Tracks preparation retries and owned client-service endpoints, keyed by tunnel name.
	tunnelExtra map[string]tunnelExtraData

	// Generation-scoped control credentials shared by the pair's state clones.
	securityConfig *dcptun.TunnelProxySecurityConfig

	// Private, generation-owned host file copied into the client container; Windows also permits SYSTEM and Administrators.
	clientSecurityFile string
}

func newContainerNetworkTunnelProxyData(
	state apiv1.ContainerNetworkTunnelProxyState,
	resourceUID types.UID,
) *containerNetworkTunnelProxyData {
	return &containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: apiv1.ContainerNetworkTunnelProxyStatus{State: state},
		resourceUID:                       resourceUID,
		startup:                           newStartupLease(),
		tunnelExtra:                       make(map[string]tunnelExtraData),
		publishedPorts:                    make(map[string]int32),
	}
}

func (tpd *containerNetworkTunnelProxyData) Clone() *containerNetworkTunnelProxyData {
	clone := containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: *tpd.ContainerNetworkTunnelProxyStatus.DeepCopy(),
		resourceUID:                       tpd.resourceUID,
		pairGeneration:                    tpd.pairGeneration,
		clientPhysicalUID:                 tpd.clientPhysicalUID,
		recovery:                          tpd.recovery,
		controlReadyAt:                    tpd.controlReadyAt,
		publishedPorts:                    std_maps.Clone(tpd.publishedPorts),
		startup:                           tpd.startup,
		serverStdout:                      tpd.serverStdout,
		serverStderr:                      tpd.serverStderr,
		tunnelExtra:                       maps.Map[string, tunnelExtraData, tunnelExtraData](tpd.tunnelExtra, tunnelExtraData.Clone),
		securityConfig:                    tpd.securityConfig,
		clientSecurityFile:                tpd.clientSecurityFile,
	}

	return &clone
}

func (tpd *containerNetworkTunnelProxyData) UpdateFrom(other *containerNetworkTunnelProxyData) bool {
	if other == nil {
		return false
	}
	if tpd.resourceUID != "" && other.resourceUID != "" && tpd.resourceUID != other.resourceUID {
		return false
	}
	if tpd.pairGeneration != other.pairGeneration {
		return false
	}

	updated := false

	if other.clientPhysicalUID != "" && tpd.clientPhysicalUID != other.clientPhysicalUID {
		tpd.clientPhysicalUID = other.clientPhysicalUID
		updated = true
	}
	if tpd.State != other.State {
		tpd.State = other.State
		updated = true
	}
	if !tpd.recovery.retryAfter.Equal(other.recovery.retryAfter) {
		tpd.recovery.retryAfter = other.recovery.retryAfter
		updated = true
	}
	if tpd.recovery.backoff == nil && other.recovery.backoff != nil {
		tpd.recovery.backoff = other.recovery.backoff
		updated = true
	}
	if tpd.recovery.neverReadyGenerations != other.recovery.neverReadyGenerations {
		tpd.recovery.neverReadyGenerations = other.recovery.neverReadyGenerations
		updated = true
	}
	if !other.controlReadyAt.IsZero() && !tpd.controlReadyAt.Equal(other.controlReadyAt) {
		tpd.controlReadyAt = other.controlReadyAt
		updated = true
	}
	if !std_maps.Equal(tpd.publishedPorts, other.publishedPorts) {
		tpd.publishedPorts = std_maps.Clone(other.publishedPorts)
		updated = true
	}

	if tpd.resourceUID == "" && other.resourceUID != "" {
		tpd.resourceUID = other.resourceUID
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

	if tpd.Message != other.Message {
		tpd.Message = other.Message
		updated = true
	}

	if tpd.serverStdout != other.serverStdout {
		tpd.serverStdout = other.serverStdout
		updated = true
	}

	if tpd.serverStderr != other.serverStderr {
		tpd.serverStderr = other.serverStderr
		updated = true
	}

	if !std_maps.EqualFunc(tpd.tunnelExtra, other.tunnelExtra, func(left tunnelExtraData, right tunnelExtraData) bool {
		return left.Equal(right)
	}) {
		tpd.tunnelExtra = maps.Map[string, tunnelExtraData, tunnelExtraData](other.tunnelExtra, tunnelExtraData.Clone)
		updated = true
	}

	if other.securityConfig != nil && tpd.securityConfig != other.securityConfig {
		tpd.securityConfig = other.securityConfig
		updated = true
	}
	if tpd.clientSecurityFile != other.clientSecurityFile {
		tpd.clientSecurityFile = other.clientSecurityFile
		updated = true
	}

	return updated
}

// Initiates ContainerNetworkTunnelProxy recovery after an operational-pair or
// replacement-startup failure; initial startup failures remain terminal.
func (tpd *containerNetworkTunnelProxyData) attemptProxyRecovery(message string) {
	if tpd.State == apiv1.ContainerNetworkTunnelProxyStateRunning ||
		(tpd.pairGeneration != 0 && (tpd.State == apiv1.ContainerNetworkTunnelProxyStateBuildingImage || tpd.State == apiv1.ContainerNetworkTunnelProxyStateStarting)) {
		tpd.State = apiv1.ContainerNetworkTunnelProxyStateRecoveryPending
	} else {
		tpd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
	}
	tpd.Message = message
}

func (tpd *containerNetworkTunnelProxyData) hasServerProxy(handle process.ProcessHandle) bool {
	return tpd.ServerProxyProcessID != nil &&
		process.Pid_t(*tpd.ServerProxyProcessID) == handle.Pid &&
		tpd.ServerProxyStartupTimestamp.Time.Equal(handle.IdentityTime)
}

func (tpd *containerNetworkTunnelProxyData) applyTo(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) objectChange {
	change := noChange

	if tpd.State != tunnelProxy.Status.State {
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

	serverStartupTime := tpd.ServerProxyStartupTimestamp.Time.Truncate(time.Microsecond)
	if !serverStartupTime.Equal(tunnelProxy.Status.ServerProxyStartupTimestamp.Time.Truncate(time.Microsecond)) {
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

	if tpd.Message != tunnelProxy.Status.Message {
		tunnelProxy.Status.Message = tpd.Message
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
	tunnelNames := slices.Map[string](tunnels, func(tc apiv1.TunnelConfiguration) string {
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
