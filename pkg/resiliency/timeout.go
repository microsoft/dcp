/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"time"
)

// Runs a function and returns when the function returns or when the specified timeout is reached.
// Returns true if the function returned before the timeout, and false if the timeout was reached.
// Note: this should not be used in a tight loop as each invocation creates a goroutine and a timer.
func RunWithTimeout(op func(), timeout time.Duration) bool {
	done := make(chan struct{}, 1)
	go func() {
		op()
		done <- struct{}{}
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
