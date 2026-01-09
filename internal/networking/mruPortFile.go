// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	std_slices "slices"
	"strings"
	"time"

	"github.com/microsoft/dcp/internal/lockfile"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	// The format of the record in the MRU ports file.
	mruPortFileRecordPattern = "%s %d %s %s" // address port timestamp instance
)

// Parameters governing the most-recently-used ports file usage.
type mruPortFileUsageParameters struct {
	// How long a recently allocated port is considered unavailable (and how long do we keep the data about such port in the MRU port file).
	// Note that a particular port number will never be re-used if it is being *actively used* by a program
	// running on the machine, even if that port has been auto-allocated earlier than "lifetime" ago.
	recentPortLifetime time.Duration

	// How long are we willing to wait for access to the MRU ports file when checking for port availability.
	portAvailableCheckTimeout time.Duration

	// How long are we willing to keep trying to allocate a non-occupied network port.
	portAllocationTimeout time.Duration

	// How many attempts to make to allocate a non-occupied port while holding a lock on the MRU ports file
	portAllocationsPerRound int

	// How long to wait (releasing the lock on the MRU ports file) before attempting to allocate a unique port again.
	portAllocationRoundDelay time.Duration

	// Used by tests to force an error if MRU port file cannot be accessed
	failOnPortFileError bool
}

func defaultMruPortFileUsageParameters() mruPortFileUsageParameters {
	var portFileParams mruPortFileUsageParameters

	portFileParams.recentPortLifetime = osutil.EnvVarDurationValWithDefault("DCP_MOST_RECENT_PORT_LIFETIME", 2*time.Minute)
	portFileParams.portAvailableCheckTimeout = osutil.EnvVarDurationValWithDefault("DCP_PORT_AVAILABILITY_CHECK_TIMEOUT", 50*time.Millisecond)
	portFileParams.portAllocationTimeout = osutil.EnvVarDurationValWithDefault("DCP_PORT_ALLOCATION_CHECK_TIMEOUT", 1*time.Second)
	portFileParams.portAllocationsPerRound = osutil.EnvVarIntValWithDefault("DCP_PORT_ALLOCATIONS_PER_ROUND", 5)
	portFileParams.portAllocationRoundDelay = osutil.EnvVarDurationValWithDefault("DCP_PORT_ALLOCATION_ROUND_DELAY", 100*time.Millisecond)

	return portFileParams
}

type mruPortFileRecord struct {
	Address        string
	Port           int32
	AddressAndPort string // Address:Port, cached for simplicity/performance
	Timestamp      time.Time
	Instance       string
}

type mruPortFileRecordMarshaller struct{}

func (_ mruPortFileRecordMarshaller) Unmarshal(line []byte) (mruPortFileRecord, error) {
	var address, timestampStr, instance string
	var port int
	n, scanErr := fmt.Fscanf(bytes.NewReader(line), mruPortFileRecordPattern, &address, &port, &timestampStr, &instance)
	if scanErr != nil {
		return mruPortFileRecord{}, fmt.Errorf("the most recently used ports file is corrupted (invalid record, the line read was '%s'): %w", line, scanErr)
	}
	if n != 4 {
		return mruPortFileRecord{}, fmt.Errorf("the most recently used ports file is corrupted (invalid record, expected 4 fields but got %d, the line was '%s')", n, line)
	}

	timestamp, timestampParseErr := time.Parse(time.RFC3339Nano, timestampStr)
	if timestampParseErr != nil {
		return mruPortFileRecord{}, fmt.Errorf("the most recently used ports file is corrupted (invalid timestamp found, the timestamp string was '%s')", timestampStr)
	}

	return mruPortFileRecord{
		Address:        strings.TrimSpace(address),
		Port:           int32(port),
		AddressAndPort: AddressAndPort(address, int32(port)),
		Timestamp:      timestamp,
		Instance:       strings.TrimSpace(instance),
	}, nil
}

func (_ mruPortFileRecordMarshaller) Marshal(record mruPortFileRecord) []byte {
	return fmt.Appendf(nil, mruPortFileRecordPattern, strings.TrimSpace(record.Address), record.Port, record.Timestamp.Format(time.RFC3339Nano), strings.TrimSpace(record.Instance))
}

type mruPortFile struct {
	lockfile.RecordFile[mruPortFileRecord]
	params mruPortFileUsageParameters
}

func newMruPortFile(path string, params mruPortFileUsageParameters) (*mruPortFile, error) {
	recordFile, err := lockfile.NewRecordFile(path, mruPortFileRecordMarshaller{})
	if err != nil {
		return nil, err
	}
	return &mruPortFile{
		RecordFile: *recordFile,
		params:     params,
	}, nil
}

// Returns the content of the most recently used ports file, with expired records removed,
// and the content sorted by (address:port) in ascending order, ready for binary search.
// The file is left locked if the operation is successful.
// If an error occurs, the file is truncated and unlocked.
func (l *mruPortFile) tryLockAndRead(ctx context.Context) ([]mruPortFileRecord, error) {
	records, readErr := l.TryLockAndRead(ctx)
	if readErr != nil {
		return nil, readErr
	}

	records = slices.Select(records, func(r mruPortFileRecord) bool {
		return time.Since(r.Timestamp) < l.params.recentPortLifetime
	})

	std_slices.SortFunc(records, func(a, b mruPortFileRecord) int {
		return cmp.Compare(a.AddressAndPort, b.AddressAndPort)
	})

	return records, nil
}

func matchAddressAndPort(a mruPortFileRecord, ap string) int {
	return cmp.Compare(a.AddressAndPort, ap)
}
