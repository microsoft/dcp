// Copyright (c) Microsoft Corporation. All rights reserved.

package lockfile

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/microsoft/dcp/pkg/osutil"
)

type RecordMarshaller[R any] interface {
	Unmarshal(line []byte) (R, error)
	Marshal(record R) []byte
}

// A RecordFile is a Lockfile that stores a sequence of records.
// Each record occupies a single line in the file.
type RecordFile[R any] struct {
	Lockfile
	rw RecordMarshaller[R]
}

func NewRecordFile[R any](path string, rw RecordMarshaller[R]) (*RecordFile[R], error) {
	if rw == nil {
		return nil, errors.New("RecordReaderWriter cannot be nil")
	}
	lockfile, err := NewLockfile(path)
	if err != nil {
		return nil, err
	}

	return &RecordFile[R]{
		Lockfile: *lockfile,
		rw:       rw,
	}, nil
}

// Reads all records from the file.
// The file is left locked if the operation is successful.
// If an error occurs, the file is truncated and unlocked.
func (rf *RecordFile[R]) TryLockAndRead(ctx context.Context) ([]R, error) {
	if rf == nil {
		return nil, errors.New("RecordFile is not initialized")
	}

	lockErr := rf.TryLock(ctx, DefaultLockRetryInterval)
	if lockErr != nil {
		return nil, lockErr
	}
	// Unlock left to the caller, unless an error occurs

	_, seekErr := rf.Seek(0, io.SeekStart)
	if seekErr != nil {
		unlockErr := rf.Unlock()
		return nil, errors.Join(seekErr, unlockErr)
	}

	// When corrupted file is detected, truncate, unlock and return an error
	cleanup := func(cause error) error {
		truncateErr := rf.Truncate(0)
		unlockErr := rf.Unlock()
		return errors.Join(cause, truncateErr, unlockErr)
	}

	var records []R
	scanner := bufio.NewScanner(rf)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		record, unmarshalErr := rf.rw.Unmarshal(line)
		if unmarshalErr != nil {
			return nil, cleanup(unmarshalErr)
		}

		records = append(records, record)
	}

	if scanner.Err() != nil {
		return nil, cleanup(fmt.Errorf("record file could not be read: %w", scanner.Err()))
	}

	return records, nil
}

// Writes the given records to the file, truncating the file first.
// The file is unlocked no matter if the operation is successful or not.
func (rf *RecordFile[R]) WriteAndUnlock(ctx context.Context, records []R) error {
	if rf == nil {
		return errors.New("RecordFile is not initialized")
	}

	// No-op if the file is already locked
	lockErr := rf.TryLock(ctx, DefaultLockRetryInterval)
	if lockErr != nil {
		return lockErr
	}

	truncateErr := rf.Truncate(0)
	if truncateErr != nil {
		return errors.Join(truncateErr, rf.Unlock())
	}

	_, seekErr := rf.Seek(0, io.SeekStart)
	if seekErr != nil {
		return errors.Join(seekErr, rf.Unlock())
	}

	for _, record := range records {
		line := rf.rw.Marshal(record)
		_, writeErr := rf.Write(osutil.WithNewline(line))
		if writeErr != nil {
			truncateErr = rf.Truncate(0)
			unlockErr := rf.Unlock()
			return errors.Join(writeErr, truncateErr, unlockErr)
		}
	}

	return rf.Unlock()
}
