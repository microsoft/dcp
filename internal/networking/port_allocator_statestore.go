/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

type stateStorePortAllocationConfig struct {
	store                 *statestore.Store
	owner                 process.ProcessTreeItem
	mode                  string
	portRanges            []portRange
	allowEphemeralOverlap bool
	allocationTimeout     time.Duration
}

const (
	defaultStateStorePortAllocationRangeStart = 20000
	defaultStateStorePortAllocationRangeEnd   = 32767
)

func getStateStorePortAllocationConfig(log logr.Logger) (*stateStorePortAllocationConfig, bool, error) {
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

	portAllocatorLock.Lock()
	store := packageStateStore
	owner := packageStateStoreOwner
	portAllocatorLock.Unlock()

	if store == nil {
		return nil, mode == portAllocatorModeStateStoreWithFallback, fmt.Errorf("state store port allocator is not configured")
	}

	params := defaultMruPortFileUsageParameters()
	allowEphemeralOverlap := osutil.EnvVarSwitchEnabled(DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
	ranges, rangesErr := configuredPortAllocationRanges(allowEphemeralOverlap, log)
	if rangesErr != nil {
		return nil, mode == portAllocatorModeStateStoreWithFallback, rangesErr
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

func allocatePortFromStateStoreRange(ctx context.Context, protocol apiv1.PortProtocol, address string, log logr.Logger) (int32, error, bool) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return 0, configErr, shouldFallback
	}

	allocationCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	candidates := newStateStorePortAllocationCandidateIterator(config.portRanges, string(protocol), address)
	for {
		if ctxErr := allocationCtx.Err(); ctxErr != nil {
			return 0, ctxErr, config.mode == portAllocatorModeStateStoreWithFallback
		}
		candidate, hasCandidate := candidates.Next()
		if !hasCandidate {
			break
		}
		reserveAttempted, reserveErr := reserveStateStoreCandidateIfBindable(allocationCtx, config, protocol, address, candidate)
		if !reserveAttempted {
			continue
		}
		if reserveErr == nil {
			return candidate, nil, false
		}
		if errors.Is(reserveErr, statestore.ErrPortReservationHeld) {
			continue
		}
		return 0, reserveErr, config.mode == portAllocatorModeStateStoreWithFallback
	}

	return 0, fmt.Errorf("could not find an available port in configured DCP port allocation range"), config.mode == portAllocatorModeStateStoreWithFallback
}

func reserveStateStoreCandidateIfBindable(
	ctx context.Context,
	config *stateStorePortAllocationConfig,
	protocol apiv1.PortProtocol,
	address string,
	port int32,
) (bool, error) {
	if lockErr := stateStorePortReservationLock.Lock(ctx); lockErr != nil {
		return true, lockErr
	}
	defer stateStorePortReservationLock.Unlock()

	if checkErr := checkPortCurrentlyBindable(protocol, address, port); checkErr != nil {
		return false, nil
	}

	_, reserveErr := config.store.ReservePort(ctx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: config.owner,
	})
	return true, reserveErr
}

func checkPortAvailableWithStateStore(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) (error, bool) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return configErr, shouldFallback
	}

	if IsEphemeralPort(port) && !config.allowEphemeralOverlap {
		return nil, false
	}

	reservationCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	if lockErr := stateStorePortReservationLock.Lock(reservationCtx); lockErr != nil {
		return lockErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	defer stateStorePortReservationLock.Unlock()

	checkErr := checkPortCurrentlyBindable(protocol, address, port)
	if checkErr != nil {
		return checkErr, false
	}

	_, reserveErr := config.store.ReserveSpecificPort(reservationCtx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: config.owner,
	})
	if reserveErr != nil {
		if errors.Is(reserveErr, statestore.ErrPortReservationHeld) {
			return fmt.Errorf("port %d is already reserved by another DCP process", port), false
		}
		return reserveErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	return nil, false
}

func reserveSpecificPortWithStateStore(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) (error, bool) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return configErr, shouldFallback
	}

	if IsEphemeralPort(port) && !config.allowEphemeralOverlap {
		return nil, false
	}

	reservationCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	if lockErr := stateStorePortReservationLock.Lock(reservationCtx); lockErr != nil {
		return lockErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	defer stateStorePortReservationLock.Unlock()

	_, reserveErr := config.store.ReserveSpecificPort(reservationCtx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: config.owner,
	})
	if reserveErr != nil {
		if errors.Is(reserveErr, statestore.ErrPortReservationHeld) {
			return fmt.Errorf("port %d is already reserved by another DCP process", port), false
		}
		return reserveErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	return nil, false
}

func releaseSpecificPortWithStateStore(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) (error, bool) {
	config, shouldFallback, configErr := getStateStorePortAllocationConfig(log)
	if configErr != nil {
		return configErr, shouldFallback
	}

	if IsEphemeralPort(port) && !config.allowEphemeralOverlap {
		return nil, false
	}

	releaseCtx, cancel := context.WithTimeout(ctx, config.allocationTimeout)
	defer cancel()

	if lockErr := stateStorePortReservationLock.Lock(releaseCtx); lockErr != nil {
		return lockErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	defer stateStorePortReservationLock.Unlock()

	releaseErr := config.store.ReleasePort(releaseCtx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: config.owner,
	})
	if releaseErr != nil {
		return releaseErr, config.mode == portAllocatorModeStateStoreWithFallback
	}
	return nil, false
}

func configuredPortAllocationRanges(allowEphemeralOverlap bool, log logr.Logger) ([]portRange, error) {
	configuredRange := strings.TrimSpace(os.Getenv(DCP_PORT_ALLOCATION_RANGE))
	// Default to the upper half of the registered port range: it has a low chance of colliding with
	// well-known service ports and stays below the default ephemeral range on almost all supported OSes.
	ranges := []portRange{{Start: defaultStateStorePortAllocationRangeStart, End: defaultStateStorePortAllocationRangeEnd}}
	if configuredRange != "" {
		parsedRanges, parseErr := parsePortAllocationRanges(configuredRange)
		if parseErr != nil {
			return nil, parseErr
		}
		ranges = parsedRanges
	}

	if allowEphemeralOverlap {
		return ranges, nil
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
		return nil, fmt.Errorf("configured DCP port allocation range overlaps the system ephemeral port range %d-%d; set %s=true to allow overlap", ephemeralStart, ephemeralEnd, DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
	}
	return filteredRanges, nil
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
	portAllocatorLock.Lock()
	defer portAllocatorLock.Unlock()
	if portRangeOverlapReported {
		return
	}
	portRangeOverlapReported = true
	log.Info("Configured DCP port allocation range overlaps the system ephemeral port range; overlapping ports will be ignored",
		"EphemeralRange", fmt.Sprintf("%d-%d", ephemeralStart, ephemeralEnd),
		"AllowOverlapEnvVar", DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP)
}

type stateStorePortAllocationCandidateIterator struct {
	ranges []portRange
	total  int
	offset int
	step   int
	next   int
}

// newStateStorePortAllocationCandidateIterator creates a deterministic random walk over the
// configured ranges. The walk is lazy so large ranges do not need to be materialized before the
// allocator finds an available port.
func newStateStorePortAllocationCandidateIterator(ranges []portRange, protocol string, address string) *stateStorePortAllocationCandidateIterator {
	total := 0
	for _, candidateRange := range ranges {
		total += candidateRange.End - candidateRange.Start + 1
	}
	if total <= 0 {
		return &stateStorePortAllocationCandidateIterator{}
	}

	hash := fnv.New64a()
	_, _ = hash.Write([]byte(programInstanceID))
	_, _ = hash.Write([]byte(protocol))
	_, _ = hash.Write([]byte(address))
	hashValue := hash.Sum64()
	offset := int(hashValue % uint64(total))
	step := stateStorePortAllocationCandidateStep(total, hashValue/uint64(total))

	return &stateStorePortAllocationCandidateIterator{
		ranges: ranges,
		total:  total,
		offset: offset,
		step:   step,
	}
}

// Next returns the next candidate port from the virtual flattened port range.
func (iterator *stateStorePortAllocationCandidateIterator) Next() (int32, bool) {
	if iterator.next >= iterator.total {
		return 0, false
	}

	candidateIndex := (iterator.offset + iterator.next*iterator.step) % iterator.total
	iterator.next++
	for _, candidateRange := range iterator.ranges {
		rangeLength := candidateRange.End - candidateRange.Start + 1
		if candidateIndex < rangeLength {
			return int32(candidateRange.Start + candidateIndex), true
		}
		candidateIndex -= rangeLength
	}

	return 0, false
}

// stateStorePortAllocationCandidateStep returns a pseudo-random step that is coprime with the
// number of candidates. A coprime step guarantees the modular walk visits every candidate exactly
// once instead of cycling through a subset of the range.
func stateStorePortAllocationCandidateStep(total int, hashValue uint64) int {
	if total <= 1 {
		return 1
	}

	step := int(hashValue%uint64(total-1)) + 1
	for gcd(step, total) != 1 {
		step++
		if step >= total {
			step = 1
		}
	}
	return step
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
