package resiliency

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// Try calling factory function with exponential back-off until a value is successfully created or timeout is reached.
func RetryGetExponential[T any](ctx context.Context, factory func() (T, error)) (T, error) {
	return RetryGet(ctx, backoff.NewExponentialBackOff(), factory)
}

// Try calling factory function with given backoff policy until a value is successfully created or timeout is reached.
func RetryGet[T any](ctx context.Context, b backoff.BackOff, factory func() (T, error)) (T, error) {
	var lastAttemptErr error

	retval, err := backoff.RetryNotifyWithData(
		factory,
		backoff.WithContext(b, ctx),
		func(err error, d time.Duration) {
			lastAttemptErr = err
		},
	)

	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
		// Inform the caller about the timeout AND the last attempt error.
		return *new(T), errors.Join(lastAttemptErr, err)
	case err != nil:
		return *new(T), err
	default:
		return retval, nil
	}
}

// Try calling operation function with exponential back-off until it succeeds or timeout is reached.
func RetryExponential(ctx context.Context, operation func() error) error {
	return Retry(ctx, backoff.NewExponentialBackOff(), operation)
}

// Try calling operation function with exponential back-off until it succeeds or timeout is reached.
func Retry(ctx context.Context, b backoff.BackOff, operation func() error) error {
	var lastAttemptErr error

	err := backoff.RetryNotify(
		operation,
		backoff.WithContext(b, ctx),
		func(err error, d time.Duration) {
			lastAttemptErr = err
		},
	)

	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
		// Inform the caller about the timeout AND the last attempt error.
		return errors.Join(lastAttemptErr, err)
	case err != nil:
		return err
	default:
		return nil
	}
}

// Creates a permanent error that stops the retry loop.
func Permanent(err error) error {
	return backoff.Permanent(err)
}
