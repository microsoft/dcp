/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package grpcutil

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/cenkalti/backoff/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Make a pointer from an enum value, for the purpose of creating protobuf messages.
// Compare with the wrappers in the google.golang.org/protobuf/proto package.
func EnumVal[T ~int32](v T) *T {
	return &v
}

// Returns true if the error is one of the errors that can happen if a gRPC stream
// is ended by the client, or the associated context is cancelled during send/receive operation.
func IsStreamDoneErr(err error) bool {
	if err == io.EOF {
		return true
	}

	if errors.Is(err, context.Canceled) {
		return true
	}

	if st, ok := status.FromError(err); ok {
		// Contrary to what the gRPC documentation says, Unavailable is non-transient when it comes to streams.
		// It is returned when the other side of the stream closes it (typically wrapping an io.EOF).
		if st.Code() == codes.Canceled || st.Code() == codes.Unavailable {
			return true
		}
	}

	return false
}

// Returns exponential backoff policy suitable for retrying send/receive operations on gRPC streams.
func StreamRetryBackoff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(100*time.Millisecond),
		backoff.WithMaxInterval(1*time.Second),
		backoff.WithMaxElapsedTime(0), // Generally we should not stop retrying until lifetime context is cancelled
	)
}
