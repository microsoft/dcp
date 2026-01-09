/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/proxy"
)

type TestProxy struct {
	lifetimeCtx context.Context
	inner       proxy.Proxy
	state       *proxy.ProxyState
	lock        *sync.Mutex
}

// Data structure to keep track of how many times a proxy start should fail,
// and how many times the ServiceReconciler has attempted to start the proxy for a given port.
type StartFailureData struct {
	RemainingFailures uint32
	Attempts          uint32
}

var (
	// How many times a proxy start should fail for a given port
	// (the value is decremented at each try)
	startFailuresByPort = make(map[int32]StartFailureData)
	startFailuresLock   = &sync.Mutex{}
)

func NewTestProxy(mode apiv1.PortProtocol, listenAddress string, listenPort int32, lifetimeCtx context.Context, log logr.Logger) proxy.Proxy {
	return &TestProxy{
		lifetimeCtx: lifetimeCtx,
		inner:       proxy.NewRuntimeProxy(mode, listenAddress, listenPort, lifetimeCtx, log),
		lock:        &sync.Mutex{},
	}
}

func InjectProxyStartFailures(port int32, failures uint32) {
	startFailuresLock.Lock()
	defer startFailuresLock.Unlock()

	sf := startFailuresByPort[port] // Zero value is fine
	sf.RemainingFailures += failures
	startFailuresByPort[port] = sf
}

func GetStartFailureData(port int32) (StartFailureData, bool) {
	startFailuresLock.Lock()
	defer startFailuresLock.Unlock()

	sf, found := startFailuresByPort[port]
	return sf, found
}

func (tp *TestProxy) Start() error {
	tp.lock.Lock()
	defer tp.lock.Unlock()

	if tp.lifetimeCtx.Err() != nil {
		tp.setState(proxy.ProxyStateFinished)
		return fmt.Errorf("proxy cannot be started: lifetime context expired: %w", tp.lifetimeCtx.Err())
	}

	state := tp.getState()
	if state != proxy.ProxyStateInitial {
		return fmt.Errorf("proxy cannot be started: invalid current state: %s", state)
	}

	startFailuresLock.Lock()
	defer startFailuresLock.Unlock()
	sf, found := startFailuresByPort[tp.ListenPort()]
	if found && sf.RemainingFailures > 0 {
		sf.RemainingFailures--
		sf.Attempts++
		startFailuresByPort[tp.ListenPort()] = sf
		return fmt.Errorf("simulating proxy start failure; remaining failures %d, attempts so far %d", sf.RemainingFailures, sf.Attempts)
	}

	tp.setState(proxy.ProxyStateRunning)
	context.AfterFunc(tp.lifetimeCtx, func() {
		tp.lock.Lock()
		defer tp.lock.Unlock()
		tp.setState(proxy.ProxyStateFinished)
	})

	// Start is a no-op for the test proxy; we do not want to open any endpoints/allocate ports etc. during test run.
	return nil
}

func (tp *TestProxy) Configure(config proxy.ProxyConfig) error {
	tp.lock.Lock()
	defer tp.lock.Unlock()

	return tp.inner.Configure(config)
}

func (tp *TestProxy) State() proxy.ProxyState {
	tp.lock.Lock()
	defer tp.lock.Unlock()

	return tp.getState()
}

func (tp *TestProxy) ListenAddress() string {
	return tp.inner.ListenAddress()
}

func (tp *TestProxy) ListenPort() int32 {
	return tp.inner.ListenPort()
}

func (tp *TestProxy) EffectiveAddress() string {
	return tp.inner.ListenAddress() // For the test proxy, the effective address is the same as the listen address
}

func (tp *TestProxy) EffectivePort() int32 {
	return tp.inner.ListenPort() // For the test proxy, the effective port is the same as the listen port
}

func (tp *TestProxy) setState(state proxy.ProxyState) {
	if tp.state == nil {
		tp.state = new(proxy.ProxyState)
	}
	*tp.state = state
}

func (tp *TestProxy) getState() proxy.ProxyState {
	if tp.lifetimeCtx.Err() != nil {
		tp.setState(proxy.ProxyStateFinished)
	}

	if tp.state == nil {
		return tp.inner.State()
	} else {
		return *tp.state
	}
}

var _ proxy.Proxy = &TestProxy{}
