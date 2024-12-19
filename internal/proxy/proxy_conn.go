// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
)

const (
	MaxOutstandingWrites      = 16
	WriteQueueFullRetryPeriod = 100 * time.Millisecond
)

var (
	Never = time.Time{}
)

type netProxyConn struct {
	// Lifetime context for the netProxyConn.
	lifetimeCtx context.Context

	// The underlying network connection.
	conn DeadlineReaderWriter

	// Channel for writeChan data (data written to the connection), its associated context and cancellation function
	writeChan          *concurrency.UnboundedChan[[]byte]
	writeChanCtx       context.Context
	writeChanCtxCancel context.CancelFunc

	// The "done" channel
	doneCh chan struct{}

	// The event to signal that the connection should be drained and stopped
	shutdownEvent *concurrency.AutoResetEvent

	// Atomic pointer to store the connection-terminating result
	result atomic.Pointer[NetworkStreamResult]

	// Buffer pool to be used for I/O operations
	bufferPool *genericPool[[]byte]

	// Counter of items to be written
	writeItems *atomic.Int32

	// Lock for operations that need synchronization
	lock *sync.Mutex
}

// Creates a new netProxyConn instance.
// Assumes the wrapped connection is already established.
func newNetProxyConn(
	lifetimeCtx context.Context,
	conn DeadlineReaderWriter,
	bufferPool *genericPool[[]byte],
) *netProxyConn {
	// We use separate context for outgoing channel to control when the channel stops pumping data.
	// Also, when the unbounded channel context is cancelled, the channel will close its "Out" part.
	writeChanCtx, writeChanCtxCancel := context.WithCancel(context.Background())

	retval := &netProxyConn{
		lifetimeCtx:        lifetimeCtx,
		conn:               conn,
		writeChan:          concurrency.NewUnboundedChan[[]byte](writeChanCtx),
		writeChanCtx:       writeChanCtx,
		writeChanCtxCancel: writeChanCtxCancel,
		doneCh:             make(chan struct{}),
		shutdownEvent:      concurrency.NewAutoResetEvent(false),
		bufferPool:         bufferPool,
		writeItems:         &atomic.Int32{},
		lock:               &sync.Mutex{},
	}

	context.AfterFunc(lifetimeCtx, retval.shutdown)

	return retval
}

func (c *netProxyConn) QueueWrite(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	if c.writeChan.BufLen() > MaxOutstandingWrites {
		return ErrWriteQueueFull
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	if c.shutdownEvent.Frozen() {
		return ErrConnectionClosed
	}

	c.writeChan.In <- data

	// Interrupt the read operation (if in progress).
	c.writeItems.Add(1)
	deadlineErr := c.conn.SetReadDeadline(time.Now())

	return deadlineErr
}

func (c *netProxyConn) Done() <-chan struct{} {
	return c.doneCh
}

func (c *netProxyConn) Result() *NetworkStreamResult {
	return c.result.Load()
}

func (c *netProxyConn) DrainAndStop() {
	c.shutdown()
	<-c.doneCh
}

// Runs the I/O loop for the netProxyConn.
func (c *netProxyConn) Run(other ProxyConn) {
	nsr := &NetworkStreamResult{}
	writes := c.writeChan.Out
	closeWriteInOnce := sync.OnceFunc(func() {
		c.lock.Lock()
		defer c.lock.Unlock()
		c.shutdownEvent.SetAndFreeze()
		close(c.writeChan.In)
	})

	defer func() {
		closeWriteInOnce()
		c.result.Store(nsr)
		c.writeChanCtxCancel()
		close(c.doneCh)
		other.DrainAndStop()
	}()

readWriteLoop:
	for {
		select {

		case <-c.shutdownEvent.Wait():
			if c.lifetimeCtx.Err() != nil {
				return // hard stop
			} else {
				break readWriteLoop
			}

		case data, isOpen := <-writes:
			c.writeItems.Add(-1)
			if !isOpen {
				// No more writes are expected
				writes = nil
				continue // We still may have some reads to do
			} else {
				keepRunning := c.writeData(data, nsr)
				c.bufferPool.Put(data[:cap(data)])
				if !keepRunning {
					return // Do not try to do any other writes, hard stop
				}
			}

		default:
			// Handling reads by "default" case means that writes preempt reads, which is what we want.
			keepRunning := c.readData(other, nsr)
			if !keepRunning {
				break readWriteLoop
			}
		}
	}

	if writes == nil {
		return // No pending writes
	}

	if c.lifetimeCtx.Err() != nil {
		// The lifetime context was cancelled, stop immediately
		return
	}

	// Ensure that no further writes are accepted
	closeWriteInOnce()

	// Try to complete any pending writes, but do not read any data anymore.
	for {
		data, isOpen := <-writes
		c.writeItems.Add(-1)
		if !isOpen {
			return
		} else {
			keepRunning := c.writeData(data, nsr)
			c.bufferPool.Put(data[:cap(data)])
			if !keepRunning {
				return
			}
		}
	}
}

func (c *netProxyConn) shutdown() {
	c.shutdownEvent.SetAndFreeze()

	// Interrupt the current i/o operation (if any)
	// We want to do this every time shutdown() is called
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.lifetimeCtx.Err() != nil {
		// Interrupt both reads and writes
		_ = c.conn.SetDeadline(time.Now())
	} else {
		// Interrupt only reads
		_ = c.conn.SetReadDeadline(time.Now())
	}
}

// Reads data from the connection, and if any data is read, schedules the write on the other connection.
// Returns a boolean indicating whether the connection is still healthy and the read-write loop should continue.
// If the read was unsuccessful, the error information is stored in the NetworkStreamResult.
func (c *netProxyConn) readData(other ProxyConn, nsr *NetworkStreamResult) bool {
	c.lock.Lock()

	// Do not read if there are pending writes
	if c.writeItems.Load() > 0 {
		c.lock.Unlock()
		return c.lifetimeCtx.Err() == nil
	}

	// Need to reset the deadline because it is "sticky" and might have been set
	// to notify us that a write is pending.
	deadlineErr := c.conn.SetReadDeadline(Never)
	if deadlineErr != nil {
		c.lock.Unlock()
		nsr.ReadErrorTimestamp = time.Now()
		nsr.ReadError = deadlineErr
		return false
	}

	c.lock.Unlock()
	buf := c.bufferPool.Get()

	in, readErr := c.conn.Read(buf)
	nsr.BytesRead += int64(in)
	keepRunning := true

	if in > 0 {
		var writeErr error

		for {
			writeErr = other.QueueWrite(buf[:in])
			if errors.Is(writeErr, ErrWriteQueueFull) && c.lifetimeCtx.Err() == nil {
				time.Sleep(WriteQueueFullRetryPeriod)
				continue
			} else {
				break
			}
		}

		if writeErr != nil {
			nsr.WriteErrorTimestamp = time.Now()
			nsr.WriteError = errors.Join(writeErr, c.lifetimeCtx.Err())
			c.bufferPool.Put(buf[:cap(buf)])
			keepRunning = false
		}
	} else {
		c.bufferPool.Put(buf[:cap(buf)])
	}

	switch {
	case readErr == nil:
		nsr.LastSuccessfulReadTimestamp = time.Now()
		return keepRunning

	case errors.Is(readErr, os.ErrDeadlineExceeded):
		return keepRunning && !c.shutdownEvent.Frozen()

	case errors.Is(readErr, io.EOF):
		return false

	default:
		nsr.ReadErrorTimestamp = time.Now()
		nsr.ReadError = readErr
		return false
	}
}

// Writes data to the connection, retrying as necessary if a partial write occurs.
// Returns true if the write was successful, otherwise false.
// If the write was unsuccessful, the error information is stored in the NetworkStreamResult.
func (c *netProxyConn) writeData(data []byte, nsr *NetworkStreamResult) bool {
	in := len(data)
	if in == 0 {
		return true
	}
	total := 0

	for total < in {
		c.lock.Lock()

		if c.lifetimeCtx.Err() != nil {
			c.lock.Unlock()
			if total > 0 {
				nsr.WriteErrorTimestamp = time.Now()
				nsr.WriteError = io.ErrShortWrite
			}
			return false
		}

		// Write deadline is sticky, need to reset it here.
		deadlineErr := c.conn.SetWriteDeadline(Never)
		if deadlineErr != nil {
			c.lock.Unlock()
			nsr.WriteErrorTimestamp = time.Now()
			nsr.WriteError = deadlineErr
			return false
		}

		c.lock.Unlock()

		written, writeErr := c.conn.Write(data[total:])
		nsr.BytesWritten += int64(written)
		total += written

		if total > in {
			nsr.WriteErrorTimestamp = time.Now()
			nsr.WriteError = ErrInconsistentWrite{written: total, expected: in}
			return false
		}
		if total == in {
			// If we wrote all the data, we do not care about the error
			break
		}

		if writeErr == nil || errors.Is(writeErr, os.ErrDeadlineExceeded) {
			if c.lifetimeCtx.Err() != nil {
				if total > 0 {
					nsr.WriteErrorTimestamp = time.Now()
					nsr.WriteError = io.ErrShortWrite
				}
				return false
			} else {
				continue
			}
		}

		// Encountered a connection-terminating error
		nsr.WriteErrorTimestamp = time.Now()
		nsr.WriteError = errors.Join(writeErr, io.ErrShortWrite)
		return false
	}

	nsr.LastSuccessfulWriteTimestamp = time.Now()
	return true
}

var _ ProxyConn = &netProxyConn{}

type ErrInconsistentWrite struct {
	written  int
	expected int
}

func (e ErrInconsistentWrite) Error() string {
	return fmt.Sprintf("inconsistent write: wrote %d bytes, expected %d", e.written, e.expected)
}

var ErrConnectionClosed = errors.New("connection closed")
