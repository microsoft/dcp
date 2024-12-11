package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
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

// Streams data between two network connections, both ways, until an error occurs with either connection.
// Returns the NetworkStreamResult for each connection.
func StreamNetworkData(
	ctx context.Context,
	east, west DeadlineReaderWriter,
	bufferPool *genericPool[[]byte],
) (*NetworkStreamResult, *NetworkStreamResult) {
	var eastConn ProxyConn = newNetProxyConn(ctx, east, bufferPool)
	var westConn ProxyConn = newNetProxyConn(ctx, west, bufferPool)

	go eastConn.Run(westConn)
	go westConn.Run(eastConn)

	<-eastConn.Done()
	<-westConn.Done()
	return eastConn.Result(), westConn.Result()
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
