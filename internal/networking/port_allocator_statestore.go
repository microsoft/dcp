/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/process"
)

type stateStorePortAllocationConfig struct {
	store                 *statestore.Store
	owner                 process.ProcessHandle
	mode                  string
	portRanges            indexedPortRanges
	allowEphemeralOverlap bool
	allocationTimeout     time.Duration
	scans                 map[portAllocationScope]*stateStorePortAllocationScan // Protected by stateStorePortReservationLock.
}

type portAllocationScope struct {
	protocol commonapi.PortProtocol
	ip       netip.Addr
}

const (
	defaultStateStorePortAllocationRangeStart = 20000
	defaultStateStorePortAllocationRangeEnd   = 32767
)

// stateStorePortReservationLock protects shared scans and serializes bind probes with reservation writes.
// Do not hold portAllocatorLock while acquiring stateStorePortReservationLock, or vice versa.
var stateStorePortReservationLock = concurrency.NewContextAwareLock()

func getStateStorePortAllocationConfig(log logr.Logger) (*stateStorePortAllocationConfig, bool, error) {
	portAllocatorLock.Lock()
	defer portAllocatorLock.Unlock()
	if !stateStoreConfigLoaded {
		stateStoreConfig, stateStoreConfigFallback, stateStoreConfigErr = loadStateStorePortAllocationConfig(log)
		stateStoreConfigLoaded = true
	}
	return stateStoreConfig, stateStoreConfigFallback, stateStoreConfigErr
}

func loadStateStorePortAllocationConfig(log logr.Logger) (*stateStorePortAllocationConfig, bool, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(DCP_PORT_ALLOCATOR)))
	if mode == "" {
		mode = portAllocatorModeStateStoreWithFallback
	}
	switch mode {
	case portAllocatorModeMru:
		return nil, true, fmt.Errorf("state store port allocation is disabled")
	case portAllocatorModeStateStore, portAllocatorModeStateStoreWithFallback:
	default:
		return nil, false, fmt.Errorf("unsupported port allocator mode '%s'", mode)
	}

	store := packageStateStore
	owner := packageStateStoreOwner
	shouldFallback := mode == portAllocatorModeStateStoreWithFallback

	if store == nil {
		return nil, shouldFallback, fmt.Errorf("state store port allocator is not configured")
	}

	params := defaultMruPortFileUsageParameters()
	allowEphemeralOverlap := osutil.EnvVarSwitchEnabled(DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
	ranges, rangesErr := configuredPortAllocationRanges(allowEphemeralOverlap, log)
	if rangesErr != nil {
		return nil, shouldFallback, rangesErr
	}

	return &stateStorePortAllocationConfig{
		store:                 store,
		owner:                 owner,
		mode:                  mode,
		portRanges:            ranges,
		allowEphemeralOverlap: allowEphemeralOverlap,
		allocationTimeout:     params.portAllocationTimeout,
	}, false, nil
}

func allocatePortFromStateStoreRange(ctx context.Context, protocol commonapi.PortProtocol, address string, log logr.Logger) (int32, *stateStorePortAllocationConfig, bool, error) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return 0, nil, shouldFallback, configErr
	}
	shouldFallback = config.mode == portAllocatorModeStateStoreWithFallback

	allocationCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	ip, parseErr := netip.ParseAddr(ToStandaloneAddress(address))
	if parseErr != nil {
		return 0, nil, shouldFallback, fmt.Errorf("could not parse port allocation IP '%s': %w", address, parseErr)
	}
	ip = ip.Unmap()
	if lockErr := stateStorePortReservationLock.Lock(allocationCtx); lockErr != nil {
		return 0, nil, shouldFallback, lockErr
	}
	defer stateStorePortReservationLock.Unlock()

	candidates := config.getPortAllocationScan(protocol, ip)
	clear(candidates.attemptedPorts)
	for {
		if ctxErr := allocationCtx.Err(); ctxErr != nil {
			return 0, nil, shouldFallback, ctxErr
		}
		candidate, hasCandidate := candidates.Next()
		if !hasCandidate {
			break
		}
		if checkErr := checkPortCurrentlyBindable(protocol, address, candidate); checkErr != nil {
			candidates.excludedPorts[candidate] = struct{}{}
			continue
		}
		request := stateStorePortReservationRequest(ports.Binding{Protocol: string(protocol), IP: ip, Port: candidate}, config.owner)
		_, reserveErr := config.store.CreatePortReservation(allocationCtx, request)
		if reserveErr == nil {
			candidates.RecordAllocation(candidate)
			return candidate, config, false, nil
		}
		if errors.Is(reserveErr, statestore.ErrPortReservationHeld) {
			continue
		}
		return 0, nil, shouldFallback, reserveErr
	}

	return 0, nil, shouldFallback, fmt.Errorf("could not find an available port in configured DCP port allocation range")
}

func checkPortAvailableWithStateStore(ctx context.Context, protocol commonapi.PortProtocol, address string, port int32, log logr.Logger) (*stateStorePortAllocationConfig, bool, error) {
	normalizedAddress, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return nil, false, addressErr
	}
	ip, ipErr := netip.ParseAddr(ToStandaloneAddress(normalizedAddress))
	if ipErr != nil {
		return nil, false, fmt.Errorf("could not parse port reservation IP '%s': %w", normalizedAddress, ipErr)
	}
	return reserveSpecificStateStorePort(ctx, ports.Binding{Protocol: string(protocol), IP: ip, Port: port}, address, true, log)
}

func reserveSpecificStateStorePort(ctx context.Context, binding ports.Binding, bindAddress string, requireBindable bool, log logr.Logger) (*stateStorePortAllocationConfig, bool, error) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return nil, shouldFallback, configErr
	}
	shouldFallback = config.mode == portAllocatorModeStateStoreWithFallback

	reservationCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	if lockErr := stateStorePortReservationLock.Lock(reservationCtx); lockErr != nil {
		return nil, shouldFallback, lockErr
	}
	defer stateStorePortReservationLock.Unlock()

	if requireBindable {
		checkErr := checkPortCurrentlyBindable(commonapi.PortProtocol(binding.Protocol), bindAddress, binding.Port)
		if checkErr != nil {
			return nil, false, checkErr
		}
	}
	if IsEphemeralPort(binding.Port) && !config.allowEphemeralOverlap {
		return nil, false, nil
	}

	request := stateStorePortReservationRequest(binding, config.owner)
	_, reserveErr := config.store.CreateOrUpdatePortReservation(reservationCtx, request)
	if reserveErr != nil {
		if errors.Is(reserveErr, statestore.ErrPortReservationHeld) {
			return nil, false, fmt.Errorf("could not reserve %s/%s/%d: %w", binding.Protocol, binding.IP, binding.Port, reserveErr)
		}
		return nil, shouldFallback, reserveErr
	}
	candidates := config.getPortAllocationScan(commonapi.PortProtocol(binding.Protocol), binding.IP)
	candidates.reservedPorts[binding.Port] = struct{}{}
	return config, false, nil
}

func (config *stateStorePortAllocationConfig) getPortAllocationScan(protocol commonapi.PortProtocol, ip netip.Addr) *stateStorePortAllocationScan {
	scope := portAllocationScope{protocol: protocol, ip: ip.Unmap()}
	if candidates, found := config.scans[scope]; found {
		return candidates
	}
	if config.scans == nil {
		config.scans = make(map[portAllocationScope]*stateStorePortAllocationScan)
	}
	candidates := newStateStorePortAllocationScan(config.portRanges)
	config.scans[scope] = candidates
	return candidates
}

func stateStorePortReservationRequest(binding ports.Binding, owner process.ProcessHandle) statestore.PortReservationRequest {
	return statestore.PortReservationRequest{
		Binding:      binding,
		OwnerProcess: owner,
	}
}

func configuredPortAllocationRanges(allowEphemeralOverlap bool, log logr.Logger) (indexedPortRanges, error) {
	configuredRange := strings.TrimSpace(os.Getenv(DCP_PORT_ALLOCATION_RANGE))
	// Default to the upper half of the registered port range: it has a low chance of colliding with
	// well-known service ports and stays below the default ephemeral range on almost all supported OSes.
	ranges := []portRange{{Start: defaultStateStorePortAllocationRangeStart, End: defaultStateStorePortAllocationRangeEnd}}
	if configuredRange != "" {
		parsedRanges, parseErr := parsePortAllocationRanges(configuredRange)
		if parseErr != nil {
			return indexedPortRanges{}, parseErr
		}
		ranges = parsedRanges
	}

	if allowEphemeralOverlap {
		return newIndexedPortRanges(ranges), nil
	}

	ephemeralStart, ephemeralEnd, matched := GetEphemeralPortRange()
	if !matched {
		ephemeralStart = DefaultEphemeralPortRangeStart
		ephemeralEnd = DefaultEphemeralPortRangeEnd
	}
	filteredRanges, overlapped := subtractPortRange(ranges, portRange{Start: ephemeralStart, End: ephemeralEnd})
	if overlapped {
		reportPortRangeOverlapWarning(log, ephemeralStart, ephemeralEnd)
	}
	if len(filteredRanges) == 0 {
		return indexedPortRanges{}, fmt.Errorf("configured DCP port allocation range overlaps the system ephemeral port range %d-%d; set %s=true to allow overlap", ephemeralStart, ephemeralEnd, DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
	}
	return newIndexedPortRanges(filteredRanges), nil
}

func parsePortAllocationRanges(value string) ([]portRange, error) {
	parts := strings.Split(value, ",")
	ranges := make([]portRange, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bounds := strings.Split(part, "-")
		if len(bounds) != 2 {
			return nil, fmt.Errorf("invalid DCP port allocation range '%s'", part)
		}
		start, startErr := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if startErr != nil {
			return nil, fmt.Errorf("invalid DCP port allocation range start '%s': %w", bounds[0], startErr)
		}
		end, endErr := strconv.Atoi(strings.TrimSpace(bounds[1]))
		if endErr != nil {
			return nil, fmt.Errorf("invalid DCP port allocation range end '%s': %w", bounds[1], endErr)
		}
		if start > end || !IsValidPort(start) || !IsValidPort(end) {
			return nil, fmt.Errorf("invalid DCP port allocation range '%s'", part)
		}
		ranges = append(ranges, portRange{Start: start, End: end})
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("DCP port allocation range cannot be empty")
	}
	return ranges, nil
}

func subtractPortRange(ranges []portRange, excluded portRange) ([]portRange, bool) {
	filteredRanges := []portRange{}
	overlapped := false
	for _, candidate := range ranges {
		if candidate.End < excluded.Start || candidate.Start > excluded.End {
			filteredRanges = append(filteredRanges, candidate)
			continue
		}

		overlapped = true
		if candidate.Start < excluded.Start {
			filteredRanges = append(filteredRanges, portRange{Start: candidate.Start, End: excluded.Start - 1})
		}
		if candidate.End > excluded.End {
			filteredRanges = append(filteredRanges, portRange{Start: excluded.End + 1, End: candidate.End})
		}
	}
	return filteredRanges, overlapped
}

func reportPortRangeOverlapWarning(log logr.Logger, ephemeralStart int, ephemeralEnd int) {
	if portRangeOverlapReported {
		return
	}
	portRangeOverlapReported = true
	log.Info("Configured DCP port allocation range overlaps the system ephemeral port range; overlapping ports will be ignored",
		"EphemeralRange", fmt.Sprintf("%d-%d", ephemeralStart, ephemeralEnd),
		"AllowOverlapEnvVar", DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
}

type indexedPortRanges struct {
	ranges []portRange
	total  int
}

func newIndexedPortRanges(ranges []portRange) indexedPortRanges {
	if len(ranges) == 0 {
		return indexedPortRanges{}
	}

	normalizedRanges := append([]portRange{}, ranges...)
	sort.Slice(normalizedRanges, func(i int, j int) bool {
		if normalizedRanges[i].Start == normalizedRanges[j].Start {
			return normalizedRanges[i].End < normalizedRanges[j].End
		}
		return normalizedRanges[i].Start < normalizedRanges[j].Start
	})

	mergedRanges := make([]portRange, 0, len(normalizedRanges))
	for _, candidateRange := range normalizedRanges {
		if len(mergedRanges) == 0 {
			mergedRanges = append(mergedRanges, candidateRange)
			continue
		}

		lastRangeIndex := len(mergedRanges) - 1
		if candidateRange.Start <= mergedRanges[lastRangeIndex].End+1 {
			if candidateRange.End > mergedRanges[lastRangeIndex].End {
				mergedRanges[lastRangeIndex].End = candidateRange.End
			}
			continue
		}
		mergedRanges = append(mergedRanges, candidateRange)
	}

	total := 0
	for _, candidateRange := range mergedRanges {
		total += candidateRange.End - candidateRange.Start + 1
	}

	return indexedPortRanges{
		ranges: mergedRanges,
		total:  total,
	}
}

func (ranges indexedPortRanges) Len() int {
	return ranges.total
}

func (ranges indexedPortRanges) PortAt(index int) (int32, bool) {
	if index < 0 || index >= ranges.total {
		return 0, false
	}

	candidateIndex := index
	for _, candidateRange := range ranges.ranges {
		rangeLength := candidateRange.End - candidateRange.Start + 1
		if candidateIndex < rangeLength {
			return int32(candidateRange.Start + candidateIndex), true
		}
		candidateIndex -= rangeLength
	}

	return 0, false
}

type stateStorePortAllocationScan struct {
	ranges           indexedPortRanges
	randomWalkCursor int
	randomWalkStep   int
	nextCandidate    int
	currentCandidate int
	reservedPorts    map[int32]struct{}
	excludedPorts    map[int32]struct{}
	attemptedPorts   map[int32]struct{}
}

// newStateStorePortAllocationScan creates a hybrid scan that advances incrementally after
// successful allocations and resumes an exhaustive random walk after a candidate is unavailable.
func newStateStorePortAllocationScan(ranges indexedPortRanges) *stateStorePortAllocationScan {
	scan := &stateStorePortAllocationScan{
		ranges:         ranges,
		nextCandidate:  -1,
		reservedPorts:  make(map[int32]struct{}),
		excludedPorts:  make(map[int32]struct{}),
		attemptedPorts: make(map[int32]struct{}),
	}
	if total := ranges.Len(); total > 0 {
		scan.randomWalkCursor = rand.IntN(total)
		scan.randomWalkStep = stateStorePortAllocationCandidateStep(total)
	}
	return scan
}

// Next returns a candidate that is neither reserved, excluded, nor attempted in the current allocation.
func (scan *stateStorePortAllocationScan) Next() (int32, bool) {
	total := scan.ranges.Len()
	if scan.nextCandidate >= 0 {
		start := scan.nextCandidate
		scan.nextCandidate = -1
		for offset := 0; offset < total; offset++ {
			if candidate, available := scan.takeCandidate((start + offset) % total); available {
				return candidate, true
			}
		}
		return 0, false
	}

	for attempts := 0; attempts < total; attempts++ {
		candidateIndex := scan.randomWalkCursor
		scan.randomWalkCursor = (scan.randomWalkCursor + scan.randomWalkStep) % total
		if candidate, available := scan.takeCandidate(candidateIndex); available {
			return candidate, true
		}
	}
	return 0, false
}

func (scan *stateStorePortAllocationScan) takeCandidate(index int) (int32, bool) {
	candidate, valid := scan.ranges.PortAt(index)
	if !valid {
		return 0, false
	}
	if _, reserved := scan.reservedPorts[candidate]; reserved {
		return 0, false
	}
	if _, excluded := scan.excludedPorts[candidate]; excluded {
		return 0, false
	}
	if _, attempted := scan.attemptedPorts[candidate]; attempted {
		return 0, false
	}
	scan.attemptedPorts[candidate] = struct{}{}
	scan.currentCandidate = index
	return candidate, true
}

// RecordAllocation retains the selected port and switches the next search to incremental scanning.
func (scan *stateStorePortAllocationScan) RecordAllocation(port int32) {
	scan.reservedPorts[port] = struct{}{}
	scan.nextCandidate = (scan.currentCandidate + 1) % scan.ranges.Len()
}

// stateStorePortAllocationCandidateStep returns a pseudo-random step that is coprime with the
// number of candidates. A coprime step guarantees the modular walk visits every candidate exactly
// once instead of cycling through a subset of the range.
func stateStorePortAllocationCandidateStep(total int) int {
	if total <= 1 {
		return 1
	}

	for {
		step := rand.IntN(total-1) + 1
		if gcd(step, total) == 1 {
			return step
		}
	}
}

// gcd computes the greatest common divisor using Euclid's algorithm.
func gcd(a int, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}
