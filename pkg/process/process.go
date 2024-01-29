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

type waitable_process struct {
	WaitPollInterval time.Duration
	process          *os.Process
	err              error
	waitChan         chan struct{}
	waitLock         sync.Mutex
}

func FindWaitableProcess(pid Pid_t) (*waitable_process, error) {
	foundProcess, err := FindProcess(pid)
	if err != nil {
		return nil, err
	}

	dcpProcess := &waitable_process{
		WaitPollInterval: defaultWaitPollInterval,
		process:          foundProcess,
		err:              nil,
		waitLock:         sync.Mutex{},
	}

	return dcpProcess, nil
}

func (p *waitable_process) pollingWait(ctx context.Context) {
	// Only setup a single wait loop per-process instance
	p.waitLock.Lock()
	defer p.waitLock.Unlock()

	// We should only setup the wait channel and polling once for a given waitable_process
	if p.waitChan == nil {
		p.waitChan = make(chan struct{})
		go func() {
			defer close(p.waitChan)

			_, err := p.process.Wait()
			if err == nil {
				return
			}

			var syscallErr syscall.Errno
			if found := errors.As(err, &syscallErr); found && syscallErr == syscall.ECHILD {
				timer := time.NewTimer(p.WaitPollInterval)

				for done := false; !done; {
					select {
					case <-timer.C:
						pid, pidConversionErr := IntToPidT(p.process.Pid)
						if pidConversionErr != nil {
							panic(pidConversionErr)
						}

						_, pollErr := FindProcess(pid)
						// We couldn't find the PID, so the process has exited
						if pollErr != nil {
							p.err = nil
							done = true
						} else {
							timer = time.NewTimer(p.WaitPollInterval)
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

func (p *waitable_process) Wait(ctx context.Context) error {
	p.pollingWait(ctx)

	select {
	case <-p.waitChan:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *waitable_process) Signal(signal syscall.Signal) error {
	return p.process.Signal(signal)
}

func (p *waitable_process) Kill() error {
	return p.process.Kill()
}
