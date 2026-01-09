/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

type ProcessExitInfo struct {
	PID      Pid_t
	ExitCode int32
	Err      error
}

func NewProcessExitInfo() ProcessExitInfo {
	return ProcessExitInfo{
		PID:      UnknownPID,
		ExitCode: UnknownExitCode,
		Err:      nil,
	}
}

// A simple process exit handler that writes the finished process status to a channel
type ChannelProcessExitHandler struct {
	c chan ProcessExitInfo
}

func NewChannelProcessExitHandler(c chan ProcessExitInfo) *ChannelProcessExitHandler {
	return &ChannelProcessExitHandler{
		c: c,
	}
}

func (eh *ChannelProcessExitHandler) OnProcessExited(pid Pid_t, exitCode int32, err error) {
	eh.c <- ProcessExitInfo{
		PID:      pid,
		ExitCode: exitCode,
		Err:      err,
	}
	close(eh.c)
}
