/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"time"
)

const (
	// Must be greater than 2 * signalAndWaitTimeout, because graceful stop can send
	// two signals before waiting for final process-exit confirmation.
	processStopTimeout = 15 * time.Second
)

// WithStopTimeout bounds process stopping while preserving parent cancellation.
func WithStopTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, processStopTimeout)
}

// WithDetachedStopTimeout bounds process cleanup without inheriting parent cancellation.
func WithDetachedStopTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return WithStopTimeout(context.WithoutCancel(parent))
}
