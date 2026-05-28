/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/ide"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const testIdeSessionStopProcessingTime = time.Millisecond * 200

// TestIdeSession represents the in-memory state of a single IdeSession run as
// the test client sees it.
type TestIdeSession struct {
	SessionID ide.SessionID
	Request   ide.StartSessionRequest
	Handler   ide.SessionHandler

	// Stopped is set once the session has been observed as terminated by the
	// test client (either via SimulateRunEnd or via StopSession).
	Stopped  bool
	ExitCode *int32
}

// TestIdeSessionClient is a fake ide.Client implementation suitable for integration
// tests. It records sessions, lets the test drive their lifecycle, and exposes hooks
// for injecting startup failures and asynchronous notifications.
type TestIdeSessionClient struct {
	nextSessionID atomic.Int64
	sessions      *syncmap.Map[ide.SessionID, *TestIdeSession]
	mu            sync.Mutex
	lifetimeCtx   context.Context

	// startupFailure, when non-nil, causes the next StartSession call to fail with
	// the wrapped error before any session is recorded.
	startupFailure error

	// autoStart, when true (default), automatically reports a successful start by
	// invoking ConfirmHandlerReady before StartSession returns.
	autoStart bool

	// startedSessions counts the total number of StartSession calls that completed
	// successfully (i.e. were not subjected to startupFailure).
	startedSessions atomic.Int32
}

// NewTestIdeSessionClient creates a new test client bound to the given lifetime context.
func NewTestIdeSessionClient(lifetimeCtx context.Context) *TestIdeSessionClient {
	return &TestIdeSessionClient{
		sessions:    &syncmap.Map[ide.SessionID, *TestIdeSession]{},
		lifetimeCtx: lifetimeCtx,
		autoStart:   true,
	}
}

// SetStartupFailure programs the next StartSession call to fail with the supplied error.
// It is cleared after one use.
func (c *TestIdeSessionClient) SetStartupFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startupFailure = err
}

// SetAutoStart controls whether StartSession automatically confirms the handler-ready gate.
// When false, callers can invoke session.Handler methods at the time of their choosing.
func (c *TestIdeSessionClient) SetAutoStart(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoStart = v
}

// FindSession returns the recorded TestIdeSession for the given session ID.
func (c *TestIdeSessionClient) FindSession(sessionID ide.SessionID) (*TestIdeSession, bool) {
	return c.sessions.Load(sessionID)
}

// AllSessions returns a slice of all sessions that the client has ever started.
func (c *TestIdeSessionClient) AllSessions() []*TestIdeSession {
	all := []*TestIdeSession{}
	c.sessions.Range(func(_ ide.SessionID, s *TestIdeSession) bool {
		all = append(all, s)
		return true
	})
	return all
}

// StartedCount returns the number of successfully started sessions.
func (c *TestIdeSessionClient) StartedCount() int32 {
	return c.startedSessions.Load()
}

// SimulateProcessStart synthesises an OnProcessChanged notification for the session
// with the supplied ID and PID.
func (c *TestIdeSessionClient) SimulateProcessStart(sessionID ide.SessionID, pid process.Pid_t) error {
	sess, found := c.sessions.Load(sessionID)
	if !found {
		return fmt.Errorf("session '%s' not found", sessionID)
	}
	if sess.Handler == nil {
		return fmt.Errorf("session '%s' has no handler registered", sessionID)
	}
	sess.Handler.OnProcessChanged(sessionID, pid)
	return nil
}

// SimulateLog synthesises an OnLog notification for the session.
func (c *TestIdeSessionClient) SimulateLog(sessionID ide.SessionID, isStdErr bool, message string) error {
	sess, found := c.sessions.Load(sessionID)
	if !found {
		return fmt.Errorf("session '%s' not found", sessionID)
	}
	if sess.Handler == nil {
		return fmt.Errorf("session '%s' has no handler registered", sessionID)
	}
	sess.Handler.OnLog(sessionID, isStdErr, message)
	return nil
}

// SimulateMessage synthesises an OnMessage notification for the session.
func (c *TestIdeSessionClient) SimulateMessage(sessionID ide.SessionID, level ide.MessageLevel, message string) error {
	sess, found := c.sessions.Load(sessionID)
	if !found {
		return fmt.Errorf("session '%s' not found", sessionID)
	}
	if sess.Handler == nil {
		return fmt.Errorf("session '%s' has no handler registered", sessionID)
	}
	sess.Handler.OnMessage(sessionID, level, message)
	return nil
}

// SimulateRunEnd synthesises an OnTerminated notification for the session.
func (c *TestIdeSessionClient) SimulateRunEnd(sessionID ide.SessionID, exitCode *int32) error {
	sess, found := c.sessions.Load(sessionID)
	if !found {
		return fmt.Errorf("session '%s' not found", sessionID)
	}
	if sess.Handler == nil {
		return fmt.Errorf("session '%s' has no handler registered", sessionID)
	}

	c.mu.Lock()
	if sess.Stopped {
		c.mu.Unlock()
		return fmt.Errorf("session '%s' is already stopped", sessionID)
	}
	sess.Stopped = true
	sess.ExitCode = exitCode
	c.mu.Unlock()

	sess.Handler.OnTerminated(sessionID, exitCode)
	return nil
}

// ----- ide.Client interface (subset) -----

// StartSession records a new session and (depending on autoStart) immediately invokes
// the handler-ready gate.
func (c *TestIdeSessionClient) StartSession(
	_ context.Context,
	req ide.StartSessionRequest,
	handler ide.SessionHandler,
	_ logr.Logger,
) (*ide.StartSessionResult, error) {
	c.mu.Lock()
	if c.startupFailure != nil {
		failure := c.startupFailure
		c.startupFailure = nil
		c.mu.Unlock()
		return nil, failure
	}
	autoStart := c.autoStart
	c.mu.Unlock()

	sessionID := ide.SessionID("test-session-" + strconv.FormatInt(c.nextSessionID.Add(1), 10))
	sess := &TestIdeSession{
		SessionID: sessionID,
		Request:   req,
		Handler:   handler,
	}
	c.sessions.Store(sessionID, sess)
	c.startedSessions.Add(1)

	confirmed := atomic.Bool{}
	confirmReady := func() {
		confirmed.Store(true)
	}

	result := &ide.StartSessionResult{
		SessionID:           sessionID,
		ConfirmHandlerReady: confirmReady,
	}

	if autoStart {
		confirmReady()
	}

	return result, nil
}

// StopSession marks the session as stopped and synthesises an OnTerminated notification
// (so that the reconciler receives the expected callback).
func (c *TestIdeSessionClient) StopSession(_ context.Context, sessionID ide.SessionID, _ logr.Logger) error {
	sess, found := c.sessions.Load(sessionID)
	if !found {
		return errors.New("session not found")
	}

	c.mu.Lock()
	if sess.Stopped {
		c.mu.Unlock()
		return nil
	}
	sess.Stopped = true
	c.mu.Unlock()

	// Simulate the stop taking a brief amount of time, like a real IDE would.
	select {
	case <-c.lifetimeCtx.Done():
		return c.lifetimeCtx.Err()
	case <-time.After(testIdeSessionStopProcessingTime):
	}

	if sess.Handler != nil {
		zero := int32(0)
		sess.Handler.OnTerminated(sessionID, &zero)
	}
	return nil
}

// ReleaseSession removes the session from the client's bookkeeping.
func (c *TestIdeSessionClient) ReleaseSession(_ context.Context, sessionID ide.SessionID, _ logr.Logger) error {
	c.sessions.Delete(sessionID)
	return nil
}

var _ controllers.IdeSessionClient = (*TestIdeSessionClient)(nil)
