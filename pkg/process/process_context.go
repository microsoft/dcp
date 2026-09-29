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
	// The graceful-stop timeout is shared by the root-first stop and the
	// descendants' graceful-stop phase. Descendants receive only the time left
	// after the root.
	gracefulProcessStopTimeout = 15 * time.Second

	// The default stop timeout includes the graceful phase and the final
	// force-kill cleanup phase.
	processStopTimeout = gracefulProcessStopTimeout + signalAndWaitTimeout
)

// WithStopTimeout bounds process stopping while preserving parent cancellation.
func WithStopTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, processStopTimeout)
}

// WithDetachedStopTimeout bounds process cleanup without inheriting parent cancellation.
func WithDetachedStopTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return WithStopTimeout(context.WithoutCancel(parent))
}
