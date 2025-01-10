package apiserver_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"

	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	defaultApiServerTestTimeout = 1 * time.Minute
)

func TestMain(m *testing.M) {
	log := testutil.NewLogForTesting("IntegrationTests")
	ctrl.SetLogger(log)

	var code int = 0
	defer func() {
		os.Exit(code)
	}()
	code = m.Run()
}

func TestReturnsExecutionData(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, "GET", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerShutdownResourceCleanup) {}, testutil.NewLogForTesting("TestReturnsExecutionData"))
	handler.ServeHTTP(w, req)

	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	body, bodyErr := io.ReadAll(resp.Body)
	require.NoError(t, bodyErr)
	require.JSONEq(t, `{"status":"Running", "shutdownResourceCleanup": "Full"}`, string(body))
}

func TestInvalidExecutionChanges(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerShutdownResourceCleanup) {}, testutil.NewLogForTesting("TestInvalidExecutionChanges"))
	var req *http.Request
	var w *httptest.ResponseRecorder
	var resp *http.Response

	// Not in application/merge-patch+json format
	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Running"}`))
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)

	// Request too large (over 512 bytes)
	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	var body bytes.Buffer
	body.WriteString(`{"status":"`)
	for i := 0; i < 600; i++ {
		body.WriteString("a")
	}
	body.WriteString(`"}`)
	req.Body = io.NopCloser(&body)
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

	// Request body is not JSON
	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`not valid JSON`))
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Request body is trying to set the status to invalid value (Running, Stopped, or some other invalid value)
	for _, statusVal := range []string{"Running", "Stopped", "Invalid"} {
		req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"` + statusVal + `"}`))
		w = httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		resp = w.Result()
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "Status value '%s' in API server execution patch should have caused 422 Unprocessable Entity response", statusVal)
	}

	// Request body is trying to set the shutdownResourceCleanup to invalid value
	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping","shutdownResourceCleanup":"invalid"}`))
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "shutdownResourceCleanup value 'invalid' in API server execution patch should have caused 422 Unprocessable Entity response")
}

func TestValidExecutionChanges(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	requestShutdownCalled := false
	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerShutdownResourceCleanup) { requestShutdownCalled = true }, testutil.NewLogForTesting("TestInvalidExecutionChanges"))
	var req *http.Request
	var w *httptest.ResponseRecorder
	var resp *http.Response

	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.True(t, requestShutdownCalled)

	requestShutdownCalled = false

	// Make the same request again (need to give it a new body since the previous one is read and closed).
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode) // 200 OK not 201 Created
	require.False(t, requestShutdownCalled)          // Not called again, since the shutdown is in progress
}

func TestCanSetResourceCleanupMode(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	requestedVsExpected := map[string]apiserver.ApiServerShutdownResourceCleanup{
		"None": apiserver.ApiServerResourceCleanupNone,
		"Full": apiserver.ApiServerResourceCleanupFull,
		"":     apiserver.ApiServerResourceCleanupFull,
	}

	for requested, expected := range requestedVsExpected {
		requestShutdownCalled := false
		cleanupPerformed := apiserver.ApiServerResourceCleanupNone
		handler := apiserver.NewAdminHttpHandler(func(cleanup apiserver.ApiServerShutdownResourceCleanup) {
			requestShutdownCalled = true
			cleanupPerformed = cleanup
		}, testutil.NewLogForTesting("TestCanSetResourceCleanupMode"))

		req := httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping","shutdownResourceCleanup":"` + requested + `"}`))
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		resp := w.Result()
		require.Equal(t, http.StatusCreated, resp.StatusCode, "Expected status code 201 Created for resource cleanup mode '%s', but got %d", requested, resp.StatusCode)
		require.True(t, requestShutdownCalled, "Expected requestShutdown to be called for resource cleanup mode '%s', but it was not", requested)
		require.Equal(t, expected, cleanupPerformed, "Expected resource cleanup cleanup mode '%s' to be used, but got '%s'", expected, cleanupPerformed)
	}
}

func TestCannotChangeExecutionWhenNotAuthenticated(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting("TestCannotChangeExecutionWhenNotAuthenticated"))
	require.NoError(t, startupErr, "Failed to start the API server")
	defer func() {
		serverInfo.Dispose()

		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
		}
	}()

	adminDocUrl := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument
	req, reqCreationErr := http.NewRequestWithContext(ctx, "GET", adminDocUrl, nil)
	require.NoError(t, reqCreationErr)
	req.Header.Set("Content-Type", "application/json")
	// NOTE: no Authorization header set

	client := getApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request for API server execution data")
	require.True(t, resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden, "Execution GET:expected status code 401 Unauthorized or 403 Forbidden, but got %d", resp.StatusCode)

	// Try to change the execution status
	req, reqCreationErr = http.NewRequestWithContext(ctx, "PATCH", adminDocUrl, nil)
	require.NoError(t, reqCreationErr)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))

	resp, respErr = client.Do(req)
	require.NoError(t, respErr, "Failed to submit request for changing API server execution status")
	require.True(t, resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden, "Execution PATCH: expected status code 401 Unauthorized or 403 Forbidden, but got %d", resp.StatusCode)
}

func TestCanStopApiServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting("TestCanStopApiServer"))
	require.NoError(t, startupErr, "Failed to start the API server")
	defer func() {
		// Still want to call this because disposal is not limited to stopping the API server
		serverInfo.Dispose()

		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
		}
	}()

	adminDocUrl := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument
	req, reqCreationErr := http.NewRequestWithContext(ctx, "PATCH", adminDocUrl, nil)
	require.NoError(t, reqCreationErr)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))
	req.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

	client := getApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request for API server execution data")
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	select {
	case <-serverInfo.ApiServerExited.Wait():
		t.Logf("API server exited as expected")
	case <-ctx.Done():
		t.Errorf("API server did not exit within the expected time")
	}
}

func getApiServerClient(t *testing.T, serverInfo *ctrl_testutil.ApiServerInfo) *http.Client {
	client := http.Client{}

	// Mostly need to set up the client to trust the server's certificate
	block, _ := pem.Decode(serverInfo.ClientConfig.TLSClientConfig.CAData)
	require.NotNil(t, block, "Failed to decode server certificate authority data")
	require.Equal(t, "CERTIFICATE", block.Type)
	cert, certParseErr := x509.ParseCertificate(block.Bytes)
	require.NoError(t, certParseErr, "Failed to parse server certificate authority data")
	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(cert)
	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}
	client.Transport = &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	return &client
}
