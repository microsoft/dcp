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
	// The 15-second disposal timeout is shared by the root-first stop and the
	// descendants' graceful-stop phase. Descendants receive only the time left
	// after the root; final force-kill cleanup uses a separate bounded context.
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
