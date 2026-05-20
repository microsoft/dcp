/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"sync"

	"github.com/microsoft/dcp/pkg/concurrency"
)

// ConcurrentProcessExitHandler is a process exit handler that captures the exit status of a process
// and makes it available to any consumers in a goroutine-safe way.
// Exited() channel can be used to wait for the proces exit info to become available.
type ConcurrentProcessExitHandler struct {
	exited          *concurrency.AutoResetEvent
	peiChan         chan ProcessExitInfo
	getExitInfoOnce func() ProcessExitInfo
}

func NewConcurrentProcessExitHandler() *ConcurrentProcessExitHandler {
	result := ConcurrentProcessExitHandler{
		exited:  concurrency.NewAutoResetEvent(false),
		peiChan: make(chan ProcessExitInfo, 1),
	}

	result.getExitInfoOnce = sync.OnceValue(func() ProcessExitInfo {
		return <-result.peiChan
	})

	return &result
}

func (e *ConcurrentProcessExitHandler) Exited() <-chan struct{} {
	return e.exited.Wait()
}

func (e *ConcurrentProcessExitHandler) ExitInfo() ProcessExitInfo {
	return e.getExitInfoOnce()
}

func (e *ConcurrentProcessExitHandler) OnProcessExited(pid Pid_t, exitCode int32, err error) {
	e.peiChan <- ProcessExitInfo{
		PID:      pid,
		ExitCode: exitCode,
		Err:      err,
	}
	e.exited.SetAndFreeze()
}
