// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/microsoft/dcp/pkg/logger"
)

var (
	Never = time.Time{}
)

type NetworkStreamResult struct {
	BytesRead                    int64
	BytesWritten                 int64
	LastSuccessfulReadTimestamp  time.Time
	LastSuccessfulWriteTimestamp time.Time
	ReadError                    error
	WriteError                   error
	ReadErrorTimestamp           time.Time
	WriteErrorTimestamp          time.Time
}

type networkResult struct {
	count     int64
	err       error
	timestamp time.Time
}

// Streams data between two network connections, both ways, until an error occurs with either connection.
// Returns the NetworkStreamResult for each connection.
func StreamNetworkData(
	ctx context.Context,
	east, west DeadlineReaderWriter,
) (*NetworkStreamResult, *NetworkStreamResult) {
	// Create pipes for proxying data between east and west
	// We use net.Pipe() rather than directly copying between the two connections
	// because we want to be able to differentiate between read and write errors
	// on each connection as well as determining which ends of the connection have
	// been closed in order to avoid deadlocks.
	eastWestPipe, westEastPipe := net.Pipe()

	// Create channels for tracking progress on the various pipe operations
	eastReadDone := make(chan networkResult, 1)
	defer close(eastReadDone)
	eastWriteDone := make(chan networkResult, 1)
	defer close(eastWriteDone)
	westReadDone := make(chan networkResult, 1)
	defer close(westReadDone)
	westWriteDone := make(chan networkResult, 1)
	defer close(westWriteDone)

	stop := context.AfterFunc(ctx, func() {
		eastWestPipe.Close()
		east.Close()
		westEastPipe.Close()
		west.Close()
	})
	defer stop()

	eastDeadlineErr := east.SetDeadline(Never)
	westDeadlineErr := west.SetDeadline(Never)
	eastWestDeadlineErr := eastWestPipe.SetDeadline(Never)
	westEastDeadlineErr := westEastPipe.SetDeadline(Never)

	if eastDeadlineErr != nil || westDeadlineErr != nil || eastWestDeadlineErr != nil || westEastDeadlineErr != nil {
		defer east.Close()
		defer west.Close()
		defer eastWestPipe.Close()
		defer westEastPipe.Close()

		return &NetworkStreamResult{
				ReadError:  eastDeadlineErr,
				WriteError: eastWestDeadlineErr,
			}, &NetworkStreamResult{
				ReadError:  westDeadlineErr,
				WriteError: westEastDeadlineErr,
			}
	}

	doStream := func(from io.ReadCloser, to io.WriteCloser, resultChan chan<- networkResult) {
		defer from.Close()
		defer to.Close()
		n, err := io.Copy(to, from)
		if isExpectedConnCloseErr(err) {
			err = nil
		}
		resultChan <- networkResult{
			count:     n,
			err:       err,
			timestamp: time.Now(),
		}
	}

	go doStream(east, eastWestPipe, eastReadDone)
	go doStream(west, westEastPipe, westReadDone)
	go doStream(westEastPipe, west, westWriteDone)
	go doStream(eastWestPipe, east, eastWriteDone)

	eastReadResult := <-eastReadDone
	westReadResult := <-westReadDone
	eastWriteResult := <-eastWriteDone
	westWriteResult := <-westWriteDone

	eastResult := &NetworkStreamResult{
		BytesRead:    eastReadResult.count,
		BytesWritten: eastWriteResult.count,
		ReadError:    eastReadResult.err,
		WriteError:   eastWriteResult.err,
	}

	westResult := &NetworkStreamResult{
		BytesRead:    westReadResult.count,
		BytesWritten: westWriteResult.count,
		ReadError:    westReadResult.err,
		WriteError:   westWriteResult.err,
	}

	if eastReadResult.err == nil {
		eastResult.LastSuccessfulReadTimestamp = eastReadResult.timestamp
	} else {
		eastResult.ReadErrorTimestamp = eastReadResult.timestamp
	}

	if eastWriteResult.err == nil {
		eastResult.LastSuccessfulWriteTimestamp = eastWriteResult.timestamp
	} else {
		eastResult.WriteErrorTimestamp = eastWriteResult.timestamp
	}

	if westReadResult.err == nil {
		westResult.LastSuccessfulReadTimestamp = westReadResult.timestamp
	} else {
		westResult.ReadErrorTimestamp = westReadResult.timestamp
	}
	if westWriteResult.err == nil {
		westResult.LastSuccessfulWriteTimestamp = westWriteResult.timestamp
	} else {
		westResult.WriteErrorTimestamp = westWriteResult.timestamp
	}

	return eastResult, westResult
}

func (nsr *NetworkStreamResult) LogProperties() map[string]string {
	return map[string]string{
		"BytesRead":           fmt.Sprint(nsr.BytesRead),
		"BytesWritten":        fmt.Sprint(nsr.BytesWritten),
		"LastSuccessfulRead":  logger.FriendlyTimestamp(nsr.LastSuccessfulReadTimestamp),
		"LastSuccessfulWrite": logger.FriendlyTimestamp(nsr.LastSuccessfulWriteTimestamp),
		"ReadError":           logger.FriendlyErrorString(nsr.ReadError),
		"ReadErrorTimestamp":  logger.FriendlyTimestamp(nsr.ReadErrorTimestamp),
		"WriteError":          logger.FriendlyErrorString(nsr.WriteError),
		"WriteErrorTimestamp": logger.FriendlyTimestamp(nsr.WriteErrorTimestamp),
	}
}
