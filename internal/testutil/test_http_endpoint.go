package testutil

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
)

// And endpoint for testing HTTP health probes.
// It returns "success" or "failure" response, depending on the configuration.
// The configuration can be changed at run time; initially the endpoint responds with "unhealthy" response.
type TestHttpEndpoint struct {
	enableUnhealthyResp   *atomic.Bool
	responses             []ResponseSpec
	url                   string
	unhealthyRespObserver *atomic.Value
	healthyRespObserver   *atomic.Value
}

const TestHttpEndpointPath = "/healthz"

func NewTestHttpEndpoint(lifetimeCtx context.Context) *TestHttpEndpoint {
	return NewTestHttpEndpointWithAddressAndPort(lifetimeCtx, "", networking.InvalidPort)
}

func NewTestHttpEndpointWithAddressAndPort(lifetimeCtx context.Context, address string, port int32) *TestHttpEndpoint {
	e := TestHttpEndpoint{
		enableUnhealthyResp:   &atomic.Bool{},
		unhealthyRespObserver: &atomic.Value{},
		healthyRespObserver:   &atomic.Value{},
	}

	e.enableUnhealthyResp.Store(true) // Initial response is "unhealthy".

	enableHealthyResp := &atomic.Bool{}
	enableHealthyResp.Store(true) // Always enabled, default response is "healthy".

	e.responses = []ResponseSpec{
		{
			StatusCode: http.StatusServiceUnavailable,
			Active:     e.enableUnhealthyResp,
			Observer: func(r *http.Request) {
				rawObserver := e.unhealthyRespObserver.Load()
				if rawObserver != nil {
					rawObserver.(func(*http.Request))(r)
				}
			},
		},
		{
			StatusCode: http.StatusOK,
			Active:     enableHealthyResp,
			Observer: func(r *http.Request) {
				rawObserver := e.healthyRespObserver.Load()
				if rawObserver != nil {
					rawObserver.(func(*http.Request))(r)
				}
			},
		},
	}

	if address != "" || networking.IsValidPort(int(port)) {
		e.url = ServeHttpFrom(lifetimeCtx, address, port, []RouteSpec{
			{
				Pattern:   TestHttpEndpointPath,
				Responses: e.responses,
			},
		}) + TestHttpEndpointPath
	} else {
		e.url = ServeHttp(lifetimeCtx, []RouteSpec{
			{
				Pattern:   TestHttpEndpointPath,
				Responses: e.responses,
			},
		}) + TestHttpEndpointPath
	}

	return &e
}

func (e *TestHttpEndpoint) SetOutcome(outcome apiv1.HealthProbeOutcome) {
	switch outcome {
	case apiv1.HealthProbeOutcomeSuccess:
		e.enableUnhealthyResp.Store(false)
	case apiv1.HealthProbeOutcomeFailure:
		e.enableUnhealthyResp.Store(true)
	default:
		panic(fmt.Sprintf("Unsupported health probe outcome: %s", outcome))
	}
}

func (e *TestHttpEndpoint) Url() string {
	return e.url
}

func (e *TestHttpEndpoint) SetUnhealthyResponseObserver(observer func(*http.Request)) {
	e.unhealthyRespObserver.Store(observer)
}

func (e *TestHttpEndpoint) SetHealthyResponseObserver(observer func(*http.Request)) {
	e.healthyRespObserver.Store(observer)
}

func (e *TestHttpEndpoint) AddressAndPort() (string, int32, error) {
	url, parseErr := url.Parse(e.url)
	if parseErr != nil {
		return "", networking.InvalidPort, parseErr
	}

	host := url.Hostname()

	portStr := url.Port()
	if len(portStr) > 0 {
		portVal, portErr := strconv.ParseInt(portStr, 10, 32)
		if portErr != nil {
			return "", networking.InvalidPort, portErr
		} else {
			return host, int32(portVal), nil
		}
	} else {
		// Assume default HTTP port, which is 80.
		return host, 80, nil
	}
}
