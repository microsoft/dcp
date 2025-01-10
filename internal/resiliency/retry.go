package resiliency

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func defaultExponentialBackoff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(200*time.Millisecond),
		backoff.WithMaxInterval(10*time.Second),
		backoff.WithMaxElapsedTime(2*time.Minute),
	)
}

// Try calling factory function with exponential back-off until a value is successfully created or timeout is reached.
// Try calling factory function with exponential back-off until either:
// - a value is successfully created, or
// - a permanent error occurs, or
// - passed context is cancelled.
func RetryGetExponential[T any](ctx context.Context, factory func() (T, error)) (T, error) {
	return RetryGet(ctx, defaultExponentialBackoff(), factory)
}

// Try calling factory function with given backoff policy until a value is successfully created or timeout is reached.
// Try calling factory function with given backoff policy until a value is successfully created,
// or a permanent error occurs, or the passed context is cancelled.
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
// Try calling operation function with exponential back-off until either:
// - the operation succeeds, or
// - a permanent error occurs, or
// - passed context is cancelled.
func RetryExponential(ctx context.Context, operation func() error) error {
	return Retry(ctx, defaultExponentialBackoff(), operation)
}

// Try calling operation function with exponential back-off until it succeeds or timeout is reached.
// Try calling operation function with exponential back-off until either:
// - the operation succeeds, or
// - a permanent error occurs, or
// - passed context is cancelled, or
// - timeout is reached.
func RetryExponentialWithTimeout(ctx context.Context, timeout time.Duration, operation func() error) error {
	timeoutCtx, cancelTimeoutCtx := context.WithTimeout(ctx, timeout)
	defer cancelTimeoutCtx()
	return Retry(timeoutCtx, defaultExponentialBackoff(), operation)
}

// Try calling operation function with exponential back-off until it succeeds,
// or a permanent error occurs, or the passed context is cancelled.
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

// Joins multiple errors into a single error, accounting for permanent errors.
func Join(errs ...error) error {

	// The reason why this is necessary and standard library errors.Join() is not sufficient
	// is because if some of passed errors are permanent errors, the errors.As(errs, &PermanentError)
	// (a check that the backoff library uses to stop the retry loop)
	// will unwrap the first permanent error in the chain and return it as the result of the whole attempt.
	// This leads to information loss because other errors in the chain are forgotten.

	isPermanent := false
	var retval error

	for _, err := range errs {
		var permanent *backoff.PermanentError
		if errors.As(err, &permanent) {
			isPermanent = true
			retval = errors.Join(retval, permanent.Err)
		} else {
			retval = errors.Join(retval, err)
		}
	}

	if isPermanent {
		retval = backoff.Permanent(retval)
	}

	return retval
}
