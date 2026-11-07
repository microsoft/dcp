//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"os"
	"os/signal"
	"syscall"
)

// EnableSIGPIPEErrors converts SIGPIPE termination into write errors until the returned function is called.
func EnableSIGPIPEErrors() func() {
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGPIPE)
	return func() {
		signal.Stop(signalCh)
	}
}
