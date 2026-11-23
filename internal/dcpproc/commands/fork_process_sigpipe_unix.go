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
	forkProcessSIGPIPEOnce sync.Once
	forkProcessSIGPIPECh   chan os.Signal
)

// enableForkProcessSIGPIPEErrorsForProcessLifetime converts SIGPIPE termination into write errors
// for the remainder of the cleanup-enabled, one-shot fork-process helper.
func enableForkProcessSIGPIPEErrorsForProcessLifetime() {
	forkProcessSIGPIPEOnce.Do(func() {
		forkProcessSIGPIPECh = make(chan os.Signal, 1)
		signal.Notify(forkProcessSIGPIPECh, syscall.SIGPIPE)
	})
}
