package resiliency

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// Try calling factory function with exponential back-off until timeout is reached.
func RetryGet[T any](ctx context.Context, factory func() (T, error)) (T, error) {
	var lastAttemptErr error

	retval, err := backoff.RetryNotifyWithData(
		factory,
		backoff.WithContext(backoff.NewExponentialBackOff(), ctx),
		func(err error, d time.Duration) {
			lastAttemptErr = err
		},
	)

	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		// Inform the caller about the timeout AND the last attempt error.
		return *new(T), errors.Join(lastAttemptErr, err)
	case err != nil:
		return *new(T), err
	default:
		return retval, nil
	}
}
