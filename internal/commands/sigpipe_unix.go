//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var (
	sigpipeSignalHandlingOnce sync.Once
	sigpipeCh                 chan os.Signal
)

// handleSigpipeSignalsForProcessLifetime handles SIGPIPE for the rest of the process lifetime, converting
// broken-pipe writes to EPIPE. A notification handler (rather than SIG_IGN) is used so that processes
// started via exec keep the default SIGPIPE disposition.
func handleSigpipeSignalsForProcessLifetime() {
	sigpipeSignalHandlingOnce.Do(func() {
		// The channel is never read; registering it is what changes how the Go runtime handles SIGPIPE.
		// A size-1 buffer is sufficient when notifying for one signal (see os/signal.Notify documentation).
		sigpipeCh = make(chan os.Signal, 1)
		signal.Notify(sigpipeCh, syscall.SIGPIPE)
	})
}
