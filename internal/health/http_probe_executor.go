// Copyright (c) Microsoft Corporation. All rights reserved.

package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutil "k8s.io/apimachinery/pkg/util/net"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	// The default HTTP probe timeout.
	defaultHttpProbeTimeout = 10 * time.Second

	maxBodyLength = 10 * 1024 // 10 KB
)

type httpHealthProbeDescriptor struct {
	// The HTTP probe, but with URL and headers resolved (template functions applied).
	pprobe *atomic.Pointer[apiv1.HttpProbe]

	// The lock for ensuring that only one goroutine is computing the resolved probe.
	lock *concurrency.ContextAwareLock
}

type HttpProbeExecutor struct {
	apiClient ctrl_client.Client
	log       logr.Logger
}

var (
	resolvedHttpProbes = new(syncmap.Map[healthProbeIdentifier, httpHealthProbeDescriptor])

	_ HealthProbeExecutor = (*HttpProbeExecutor)(nil)
)

func NewHttpProbeExecutor(apiClient ctrl_client.Client, log logr.Logger) *HttpProbeExecutor {
	if apiClient == nil {
		panic("apiClient cannot be nil")
	}
	return &HttpProbeExecutor{
		apiClient: apiClient,
		log:       log,
	}
}

func (hpe *HttpProbeExecutor) Execute(
	executionCtx context.Context,
	probeDefinition *apiv1.HealthProbe,
	owner commonapi.DcpModelObject,
	probeID healthProbeIdentifier,
) (apiv1.HealthProbeResult, error) {
	if probeDefinition == nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("probe is nil")
	}
	if probeDefinition.Type != apiv1.HealthProbeTypeHttp {
		return apiv1.HealthProbeResult{}, fmt.Errorf("probe type is not HTTP")
	}
	if probeDefinition.HttpProbe == nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("HTTP probe data is missing")
	}

	timeout := defaultHttpProbeTimeout
	if probeDefinition.Schedule.Timeout != nil && probeDefinition.Schedule.Timeout.Duration > 0 {
		timeout = probeDefinition.Schedule.Timeout.Duration
	}

	dialer := &net.Dialer{
		Timeout: timeout,
	}

	transport := http.Transport{
		Proxy:              netutil.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment),
		DialContext:        dialer.DialContext,
		DisableKeepAlives:  true,
		DisableCompression: true, // Removes Accept-Encoding: gzip header
	}
	client := http.Client{
		Transport: &transport,
		Timeout:   timeout,
	}

	probe, resolutionErr := hpe.resolveHttpProbe(executionCtx, probeDefinition, owner, probeID)
	if resolutionErr != nil {
		finalErr := fmt.Errorf("data necessary to execute HTTP probe could not be resolved: %w", resolutionErr)
		if templating.IsTransientTemplateError(resolutionErr) {
			return unknownResult(probeDefinition.Name, finalErr), nil
		} else {
			return failureResult(probeDefinition.Name, finalErr), nil
		}
	}

	reqCtx, reqCtxCancel := context.WithTimeout(executionCtx, timeout)
	defer reqCtxCancel()

	req, reqCreationErr := http.NewRequestWithContext(reqCtx, http.MethodGet, probe.Url, nil)
	if reqCreationErr != nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("failed to create HTTP request: %w", reqCreationErr)
	}

	if len(probe.Headers) > 0 {
		for _, header := range probe.Headers {
			req.Header.Add(header.Name, header.Value)
		}
	}

	resp, respErr := client.Do(req)
	if respErr != nil {
		return failureResult(probeDefinition.Name, respErr), nil
	}
	defer resp.Body.Close()

	lr := io.LimitedReader{R: resp.Body, N: maxBodyLength}
	body, bodyErr := io.ReadAll(&lr)
	if bodyErr != nil {
		return failureResult(probeDefinition.Name, bodyErr), nil
	}

	switch {
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest:
		return failureResult(
			probeDefinition.Name,
			fmt.Errorf("HTTP probe failed with status code %d, Body: %s", resp.StatusCode, string(body)),
		), nil
	case resp.StatusCode >= http.StatusMultipleChoices:
		// Redirect
		return unknownResult(
			probeDefinition.Name,
			fmt.Errorf("HTTP probe received redirect response with status code %d, Body: %s", resp.StatusCode, string(body)),
		), nil
	default:
		return apiv1.HealthProbeResult{
			Outcome:   apiv1.HealthProbeOutcomeSuccess,
			Timestamp: metav1.NowMicro(),
			ProbeName: probeDefinition.Name,
		}, nil
	}
}

func (hpe *HttpProbeExecutor) resolveHttpProbe(
	executionCtx context.Context,
	probeDefinition *apiv1.HealthProbe,
	owner commonapi.DcpModelObject,
	probeID healthProbeIdentifier,
) (*apiv1.HttpProbe, error) {
	hpd, _ := resolvedHttpProbes.LoadOrStoreNew(probeID, func() httpHealthProbeDescriptor {
		return httpHealthProbeDescriptor{
			pprobe: new(atomic.Pointer[apiv1.HttpProbe]),
			lock:   concurrency.NewContextAwareLock(),
		}
	})

	retval := hpd.pprobe.Load()
	if retval != nil {
		return retval, nil
	}

	lockErr := hpd.lock.Lock(executionCtx)
	if lockErr != nil {
		return nil, lockErr
	}
	defer hpd.lock.Unlock()

	// Load again, in case another goroutine has already resolved the probe.
	retval = hpd.pprobe.Load()
	if retval != nil {
		return retval, nil
	}

	tmpl, tmplErr := templating.NewHealthProbeSpecValueTemplate(executionCtx, hpe.apiClient, owner, hpe.log)
	if tmplErr != nil {
		return nil, tmplErr
	}

	retval = &apiv1.HttpProbe{}
	url, tmplExecutionErr := templating.ExecuteTemplate(tmpl, owner, probeDefinition.HttpProbe.Url, "HTTP probe URL", hpe.log)
	if tmplExecutionErr != nil {
		return nil, tmplExecutionErr
	}
	retval.Url = url

	for _, header := range probeDefinition.HttpProbe.Headers {
		headerValue, headerTmplExecutionErr := templating.ExecuteTemplate(tmpl, owner, header.Value, "HTTP probe header value", hpe.log)
		if headerTmplExecutionErr != nil {
			return nil, headerTmplExecutionErr
		}
		retval.Headers = append(retval.Headers, apiv1.HttpHeader{Name: header.Name, Value: headerValue})
	}

	hpd.pprobe.Store(retval)
	return retval, nil
}

func failureResult(probeName string, err error) apiv1.HealthProbeResult {
	return apiv1.HealthProbeResult{
		Outcome:   apiv1.HealthProbeOutcomeFailure,
		Timestamp: metav1.NowMicro(),
		ProbeName: probeName,
		Reason:    err.Error(),
	}
}

func unknownResult(probeName string, err error) apiv1.HealthProbeResult {
	return apiv1.HealthProbeResult{
		Outcome:   apiv1.HealthProbeOutcomeUnknown,
		Timestamp: metav1.NowMicro(),
		ProbeName: probeName,
		Reason:    err.Error(),
	}
}
