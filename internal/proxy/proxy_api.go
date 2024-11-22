// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"bytes"
	"context"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
)

type Endpoint struct {
	Address string `yaml:"address"`
	Port    int32  `yaml:"port"`
}

type ProxyState uint32

const (
	ProxyStateInitial  ProxyState = 0x1
	ProxyStateRunning  ProxyState = 0x2
	ProxyStateFailed   ProxyState = 0x4
	ProxyStateFinished ProxyState = 0x8
	ProxyStateAny      ProxyState = 0xFFFFFFFF
)

func (s ProxyState) String() string {
	switch s {
	case ProxyStateInitial:
		return "Initial"
	case ProxyStateRunning:
		return "Running"
	case ProxyStateFailed:
		return "Failed"
	case ProxyStateFinished:
		return "Finished"
	default:
		return "Unknown"
	}
}

type ProxyConfig struct {
	Endpoints []Endpoint
}

func (pc *ProxyConfig) Clone() ProxyConfig {
	endpoints := make([]Endpoint, len(pc.Endpoints))
	copy(endpoints, pc.Endpoints)
	return ProxyConfig{Endpoints: endpoints}
}

func (pc *ProxyConfig) String() string {
	var b bytes.Buffer
	b.WriteString("[")
	for i, endpoint := range pc.Endpoints {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(networking.AddressAndPort(endpoint.Address, endpoint.Port))
	}
	b.WriteString("]")
	return b.String()
}

// Represents a reverse proxy.
//
// After Start() method is called, the proxy will listen on the specified address and port
// (which cannot be changed after the proxy is created), and forward incoming connections
// to the endpoints specified by the configuration (supplied via Configure() method).
// The proxy will stop when the lifetime context is cancelled (passed via ProxyFactory.CreateProxy()).
type Proxy interface {
	Start() error
	Configure(ProxyConfig) error
	State() ProxyState
	ListenAddress() string
	ListenPort() int32
	EffectiveAddress() string
	EffectivePort() int32
}

// Creates a reverse proxy.
//
// If the listenAddress is empty, the proxy will listen on localhost.
// The Proxy.EffectiveAddress() returns the actual IPv4 or IPv6 address used for listening.
// If the port is 0, the proxy will listen on a random port. Proxy.EffectivePort() will return the actual listened-on port.
type ProxyFactory func(
	mode apiv1.PortProtocol,
	listenAddress string,
	listenPort int32,
	lifetimeCtx context.Context,
	log logr.Logger,
) Proxy
