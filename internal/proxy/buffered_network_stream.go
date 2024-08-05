package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

type BufferedNetworkStreamResult struct {
	BytesRead                    int64
	BytesWritten                 int64
	LastSuccessfulReadTimestamp  time.Time
	LastSuccessfulWriteTimestamp time.Time
	ReadError                    error
	WriteError                   error
	ReadErrorTimestamp           time.Time
	WriteErrorTimestamp          time.Time
}

func StreamNetworkData(ctx context.Context, bufferSize int, source DeadlineReader, dest DeadlineWriter, timeout time.Duration) *BufferedNetworkStreamResult {
	bns := &BufferedNetworkStreamResult{}

	reader := bufio.NewReader(source)
	buf := make([]byte, bufferSize)
	var deadlineErr error

	for {
		// Reset the read deadline before each potential input socket read
		deadlineErr = source.SetReadDeadline(time.Now().Add(timeout))
		if deadlineErr != nil {
			bns.ReadErrorTimestamp = time.Now()
			bns.ReadError = deadlineErr
			return bns
		}

		in, readErr := reader.Read(buf)
		bns.BytesRead += int64(in)
		if readErr == nil || errors.Is(readErr, io.EOF) {
			bns.LastSuccessfulReadTimestamp = time.Now()
		} else {
			bns.ReadErrorTimestamp = time.Now()
			bns.ReadError = readErr
		}

		if in > 0 {
			// Reset the write deadline after every read so that we aren't counting time spent reading against time spent writing
			deadlineErr = dest.SetWriteDeadline(time.Now().Add(timeout))
			if deadlineErr != nil {
				bns.WriteErrorTimestamp = time.Now()
				bns.WriteError = deadlineErr
				return bns
			}

			// If we read any data, always try to write it to the destination socket
			out, writeErr := dest.Write(buf[:in])
			bns.BytesWritten += int64(out)

			if errors.Is(writeErr, os.ErrDeadlineExceeded) {
				deadlineErr = dest.SetWriteDeadline(time.Now().Add(timeout))
				if deadlineErr != nil {
					bns.WriteErrorTimestamp = time.Now()
					bns.WriteError = deadlineErr
					return bns
				}

				retryOut, retryWriteErr := dest.Write(buf[out:in])
				bns.BytesWritten += int64(retryOut)
				out += retryOut

				if retryWriteErr == nil {
					// If we recovered, ignore the previous error
					writeErr = nil
				} else {
					// If we failed on retry, report both errors
					writeErr = errors.Join(writeErr, retryWriteErr)
				}
			}

			if out != in {
				// Report a short write if we didn't write the expected amount of data
				writeErr = errors.Join(writeErr, io.ErrShortWrite)
			}

			if writeErr != nil {
				// If we encounter an unrecoverable write error, we should stop the stream
				bns.WriteErrorTimestamp = time.Now()
				bns.WriteError = writeErr
				return bns
			}

			bns.LastSuccessfulWriteTimestamp = time.Now()
		}

		if readErr != nil && !errors.Is(readErr, os.ErrDeadlineExceeded) {
			bns.ReadErrorTimestamp = time.Now()
			bns.ReadError = readErr
			// If we encounter an unrecoverable read error, we should stop the stream.
			// This can include expected errors such as io.EOF.
			return bns
		}

		select {
		case <-ctx.Done():
			// Cancellation, so stop what we're doing; report the context error as the read error
			bns.ReadErrorTimestamp = time.Now()
			bns.ReadError = ctx.Err()
			return bns
		default:
			continue
		}
	}
}

func (bns *BufferedNetworkStreamResult) LogProperties() map[string]string {
	return map[string]string{
		"BytesRead":           fmt.Sprint(bns.BytesRead),
		"BytesWritten":        fmt.Sprint(bns.BytesWritten),
		"LastSuccessfulRead":  logger.FriendlyTimestamp(bns.LastSuccessfulReadTimestamp),
		"LastSuccessfulWrite": logger.FriendlyTimestamp(bns.LastSuccessfulWriteTimestamp),
		"ReadError":           logger.FriendlyErrorString(bns.ReadError),
		"ReadErrorTimestamp":  logger.FriendlyTimestamp(bns.ReadErrorTimestamp),
		"WriteError":          logger.FriendlyErrorString(bns.WriteError),
		"WriteErrorTimestamp": logger.FriendlyTimestamp(bns.WriteErrorTimestamp),
	}
}
