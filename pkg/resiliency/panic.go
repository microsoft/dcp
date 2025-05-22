package resiliency

import (
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
)

// Logs a panic value and associated call stack and returns it as an error.
func MakePanicError(panicVal any, log logr.Logger) error {
	if panicVal == nil {
		return nil
	}

	panicErr, isError := panicVal.(error)
	if !isError {
		panicErr = fmt.Errorf("%v", panicVal)
	}
	var permanent *backoff.PermanentError
	if !errors.As(panicErr, &permanent) {
		panicErr = Permanent(panicErr)
	}

	log.Error(panicErr, "A goroutine ended prematurely due to panic", "stack", string(debug.Stack()))

	return panicErr
}
