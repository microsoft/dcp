package grpcutil

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Make a pointer from an enum value, for the purpose of creating protobuf messages.
// Compare with the wrappers in the google.golang.org/protobuf/proto package.
func EnumVal[T ~int32](v T) *T {
	return &v
}

// Returns true if the error is one of the errors that can happen if a gRPC stream
// is ended by the client, or the associated context is cancelled.
func IsStreamDoneErr(err error) bool {
	if err == io.EOF {
		return true
	}

	if errors.Is(err, context.Canceled) {
		return true
	}

	if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
		return true
	}

	return false
}
