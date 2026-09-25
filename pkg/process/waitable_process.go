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
	process          *os.Process
	handle           ProcessHandle
	err              error
	waitChan         chan struct{}
	waitLock         sync.Mutex
}

func FindWaitableProcess(handle ProcessHandle) (*WaitableProcess, error) {
	foundProcess, err := FindProcess(handle)
	if err != nil {
		return nil, err
	}

	dcpProcess := &WaitableProcess{
		WaitPollInterval: defaultWaitPollInterval,
		process:          foundProcess,
		handle:           handle,
		err:              nil,
		waitLock:         sync.Mutex{},
	}

	return dcpProcess, nil
}

func (p *WaitableProcess) Pid() Pid_t {
	return p.handle.Pid
}

func (p *WaitableProcess) IdentityTime() time.Time {
	return p.handle.IdentityTime
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
			p.err = waitForProcess(ctx, p.handle, p.process, p.WaitPollInterval)
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
	proc, findErr := FindProcess(p.handle)
	if findErr != nil {
		return findErr
	}
	signalErr := signalProcess(context.Background(), p.handle, proc, signal)
	return errors.Join(signalErr, proc.Release())
}

func (p *WaitableProcess) Kill() error {
	proc, findErr := FindProcess(p.handle)
	if findErr != nil {
		return findErr
	}
	killErr := signalProcess(context.Background(), p.handle, proc, os.Kill)
	return errors.Join(killErr, proc.Release())
}

func waitForProcess(ctx context.Context, handle ProcessHandle, proc *os.Process, interval time.Duration) error {
	_, waitErr := proc.Wait()
	if waitErr == nil {
		return nil
	}
	releaseErr := proc.Release()
	if !errors.Is(waitErr, syscall.ECHILD) {
		return errors.Join(waitErr, releaseErr)
	}
	if releaseErr != nil {
		return releaseErr
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			_, pollErr := findProcessInfo(handle)
			if IsProcessGoneErr(pollErr) {
				return nil
			}
			if pollErr != nil {
				return pollErr
			}
			timer.Reset(interval)
		}
	}
}
