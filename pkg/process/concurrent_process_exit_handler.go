/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"github.com/microsoft/dcp/pkg/concurrency"
)

// ConcurrentProcessExitHandler is a process exit handler that captures the exit status of a process
// and makes it available to any consumers in a goroutine-safe way.
// Exited() channel can be used to wait for the process exit info to become available.
type ConcurrentProcessExitHandler struct {
	p *concurrency.ValuePromise[ProcessExitInfo]
}

func NewConcurrentProcessExitHandler() *ConcurrentProcessExitHandler {
	return &ConcurrentProcessExitHandler{
		p: concurrency.NewValuePromise[ProcessExitInfo](),
	}
}

func (e *ConcurrentProcessExitHandler) Exited() <-chan struct{} {
	return e.p.Wait()
}

func (e *ConcurrentProcessExitHandler) ExitInfo() ProcessExitInfo {
	return e.p.Get()
}

func (e *ConcurrentProcessExitHandler) OnProcessExited(pid Pid_t, exitCode int32, err error) {
	e.p.Set(ProcessExitInfo{PID: pid, ExitCode: exitCode, Err: err})
}
