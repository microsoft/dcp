package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	netutil "k8s.io/apimachinery/pkg/util/net"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// The default HTTP probe timeout.
	defaultHttpProbeTimeout = 10 * time.Second

	maxBodyLength = 10 * 1024 // 10 KB
)

func ExecuteHttpProbe(executionCtx context.Context, probe *apiv1.HealthProbe) (apiv1.HealthProbeResult, error) {
	if probe == nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("probe is nil")
	}
	if probe.Type != apiv1.HealthProbeTypeHttp {
		return apiv1.HealthProbeResult{}, fmt.Errorf("probe type is not HTTP")
	}
	if probe.HttpProbe == nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("HTTP probe data is missing")
	}

	timeout := defaultHttpProbeTimeout
	if probe.Schedule.Timeout != nil && probe.Schedule.Timeout.Duration > 0 {
		timeout = probe.Schedule.Timeout.Duration
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

	reqCtx, reqCtxCancel := context.WithTimeout(executionCtx, timeout)
	defer reqCtxCancel()

	req, reqCreationErr := http.NewRequestWithContext(reqCtx, http.MethodGet, probe.HttpProbe.Url, nil)
	if reqCreationErr != nil {
		return apiv1.HealthProbeResult{}, fmt.Errorf("failed to create HTTP request: %w", reqCreationErr)
	}

	if len(probe.HttpProbe.Headers) > 0 {
		for _, header := range probe.HttpProbe.Headers {
			req.Header.Add(header.Name, header.Value)
		}
	}

	resp, respErr := client.Do(req)
	if respErr != nil {
		return failureResult(probe.Name, respErr), nil
	}
	defer resp.Body.Close()

	lr := io.LimitedReader{R: resp.Body, N: maxBodyLength}
	body, bodyErr := io.ReadAll(&lr)
	if bodyErr != nil {
		return failureResult(probe.Name, bodyErr), nil
	}

	switch {
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest:
		return failureResult(
			probe.Name,
			fmt.Errorf("HTTP probe failed with status code %d, Body: %s", resp.StatusCode, string(body)),
		), nil
	case resp.StatusCode >= http.StatusMultipleChoices:
		// Redirect
		return apiv1.HealthProbeResult{
			Outcome:   apiv1.HealthProbeOutcomeUnknown,
			Timestamp: metav1.NowMicro(),
			ProbeName: probe.Name,
			Reason:    fmt.Sprintf("HTTP probe received redirect response with status code %d, Body: %s", resp.StatusCode, string(body)),
		}, nil
	default:
		return apiv1.HealthProbeResult{
			Outcome:   apiv1.HealthProbeOutcomeSuccess,
			Timestamp: metav1.NowMicro(),
			ProbeName: probe.Name,
		}, nil
	}
}

func failureResult(probeName string, err error) apiv1.HealthProbeResult {
	return apiv1.HealthProbeResult{
		Outcome:   apiv1.HealthProbeOutcomeFailure,
		Timestamp: metav1.NowMicro(),
		ProbeName: probeName,
		Reason:    err.Error(),
	}
}
