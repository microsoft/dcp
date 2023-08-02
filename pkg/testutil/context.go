package testutil

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func GetTestContext(t *testing.T, testTimeout time.Duration) (context.Context, context.CancelFunc) {
	timeoutStr, found := os.LookupEnv("TEST_CONTEXT_TIMEOUT")
	if found {
		timeout, err := strconv.ParseUint(timeoutStr, 10, 16)
		if err != nil {
			panic(fmt.Sprintf("Context timeout value '%s' is invalid: %s", timeoutStr, err.Error()))
		}
		return context.WithTimeout(context.Background(), time.Duration(timeout)*time.Minute)
	}

	deadline, haveDeadline := t.Deadline()

	switch {
	case !haveDeadline && testTimeout == 0:
		return context.WithCancel(context.Background())

	case haveDeadline && testTimeout == 0:
		return context.WithDeadline(context.Background(), deadline)

	case !haveDeadline && testTimeout != 0:
		return context.WithTimeout(context.Background(), testTimeout)

	case haveDeadline && testTimeout != 0:
		testDeadline := time.Now().Add(testTimeout)
		// Take shorter of the two deadlines
		if testDeadline.Before(deadline) {
			return context.WithDeadline(context.Background(), testDeadline)
		} else {
			return context.WithDeadline(context.Background(), deadline)
		}

	default:
		panic("should never happen--checks above should be exhaustive")
	}
}
