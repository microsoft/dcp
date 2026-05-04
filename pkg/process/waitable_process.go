/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	defaultWaitPollInterval = time.Second * 2
)

type WaitableProcess struct {
	WaitPollInterval time.Duration
	PID              Pid_t
	ProcessStartTime time.Time
	ExitCode         int32

	process  *os.Process
	err      error
	waitChan chan struct{}
	waitLock sync.Mutex
}

func FindWaitableProcess(pid Pid_t, processStartTime time.Time) (*WaitableProcess, error) {
	foundProcess, err := FindProcess(pid, processStartTime)
	if err != nil {
		return nil, err
	}

	dcpProcess := &WaitableProcess{
		WaitPollInterval: defaultWaitPollInterval,
		PID:              pid,
		ProcessStartTime: processStartTime,
		ExitCode:         UnknownExitCode,

		process:  foundProcess,
		err:      nil,
		waitLock: sync.Mutex{},
	}

	return dcpProcess, nil
}

func (p *WaitableProcess) pollingWait(ctx context.Context) {
	// Only setup a single wait loop per-process instance
	p.waitLock.Lock()
	defer p.waitLock.Unlock()

	// We should only setup the wait channel and polling once for a given waitable_process
	if p.waitChan == nil {
		p.waitChan = make(chan struct{})
		go func() {
			defer close(p.waitChan)

			pstate, err := p.process.Wait()
			p.ExitCode, err = getProcessExecResult(err, pstate)
			if err == nil {
				return
			}

			var syscallErr syscall.Errno
			if found := errors.As(err, &syscallErr); found && syscallErr == syscall.ECHILD {

				// Most likely we are not the parent of the process, so the ordinary Wait() call does not work.
				// Could also be that we already started waiting for this process, but that would be a programming error.
				// Ether way, we can still poll-wait, we just won't get the exit code.

				timer := time.NewTimer(p.WaitPollInterval)
				defer timer.Stop()

				for done := false; !done; {
					select {
					case <-timer.C:
						pid := Uint32_ToPidT(uint32(p.process.Pid))

						_, pollErr := FindProcess(pid, p.ProcessStartTime)
						// We couldn't find the PID, so the process has exited
						if pollErr != nil {
							p.err = nil
							done = true
						} else {
							timer.Reset(p.WaitPollInterval)
						}

					case <-ctx.Done():
						p.err = ctx.Err()
						done = true
					}
				}
			} else {
				p.err = err
			}
		}()
	}
}

func (p *WaitableProcess) Wait(ctx context.Context) error {
	p.pollingWait(ctx)

	select {
	case <-p.waitChan:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *WaitableProcess) Signal(signal syscall.Signal) error {
	return p.process.Signal(signal)
}

func (p *WaitableProcess) Kill() error {
	return p.process.Kill()
}
