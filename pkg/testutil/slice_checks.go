// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/microsoft/dcp/pkg/slices"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Verify that the length of a slice does not change over several periods of time.
// The function is taking a pointer to the slice because the slice might be manipulated by other goroutines.
func LengthNotChanging[S ~[]E, E any](ctx context.Context, s *S, period time.Duration, numPeriods uint, lock sync.Locker) error {
	const invalidLength = -1

	if period < 0 {
		return errors.New("period must be non-negative")
	}
	if numPeriods == 0 {
		return errors.New("numPeriods must be positive")
	}

	if lock == nil {
		return errors.New("lock must not be nil")
	}

	observed := make([]int, numPeriods)
	for i := range observed {
		observed[i] = invalidLength
	}
	var index uint = 0

	waitErr := wait.PollUntilContextCancel(ctx, period, true /* poll immediately */, func(_ context.Context) (bool, error) {
		lock.Lock()
		defer lock.Unlock()

		currentLength := len(*s)
		observed[index] = currentLength
		index = (index + 1) % numPeriods

		return slices.All(observed, func(l int) bool { return l == currentLength }), nil
	})

	return waitErr
}
