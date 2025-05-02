package apiserver_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrl "sigs.k8s.io/controller-runtime"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
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
	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerResourceCleanup) {}, testutil.NewLogForTesting(t.Name()))
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

	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerResourceCleanup) {}, testutil.NewLogForTesting(t.Name()))
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
	for _, statusVal := range []string{"Running", "Stopped", "CleanupComplete", "Invalid"} {
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
	handler := apiserver.NewAdminHttpHandler(func(apiserver.ApiServerResourceCleanup) { requestShutdownCalled = true }, testutil.NewLogForTesting(t.Name()))
	var req *http.Request
	var w *httptest.ResponseRecorder
	var resp *http.Response

	req = httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.True(t, requestShutdownCalled)

	requestShutdownCalled = false

	// Make the same request again (need to give it a new body since the previous one is read and closed).
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusNoContent, resp.StatusCode) // 204 NoContent not 202 Accepted
	require.False(t, requestShutdownCalled)                 // Not called again, since the shutdown is in progress

	// It is OK to request a cleanup after the shutdown has started
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"CleaningResources"}`))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode) // 200 OK not 202 Accepted
	respBody, bodyErr := io.ReadAll(resp.Body)
	require.NoError(t, bodyErr)
	require.JSONEq(t, `{"status":"Stopping", "shutdownResourceCleanup":"Full"}`, string(respBody), "The server should notify the client that it is already stopping")
}

func TestCanSetResourceCleanupMode(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	requestedVsExpected := map[string]apiserver.ApiServerResourceCleanup{
		"None": apiserver.ApiServerResourceCleanupNone,
		"Full": apiserver.ApiServerResourceCleanupFull,
		"":     apiserver.ApiServerResourceCleanupFull,
	}

	for requested, expected := range requestedVsExpected {
		requestShutdownCalled := false
		cleanupPerformed := apiserver.ApiServerResourceCleanupNone
		handler := apiserver.NewAdminHttpHandler(func(cleanup apiserver.ApiServerResourceCleanup) {
			requestShutdownCalled = true
			cleanupPerformed = cleanup
		}, testutil.NewLogForTesting(t.Name()+requested))

		req := httptest.NewRequestWithContext(ctx, "PATCH", apiserver.AdminPathPrefix+apiserver.ExecutionDocument, nil)
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping","shutdownResourceCleanup":"` + requested + `"}`))
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		resp := w.Result()
		require.Equal(t, http.StatusAccepted, resp.StatusCode, "Expected status code 201 Created for resource cleanup mode '%s', but got %d", requested, resp.StatusCode)
		require.True(t, requestShutdownCalled, "Expected requestShutdown to be called for resource cleanup mode '%s', but it was not", requested)
		require.Equal(t, expected, cleanupPerformed, "Expected resource cleanup cleanup mode '%s' to be used for requested cleanup mode '%s', but got '%s'", expected, requested, cleanupPerformed)
	}
}

func TestCannotChangeExecutionWhenNotAuthenticated(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting(t.Name()))
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

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
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

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting(t.Name()))
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

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to stop the API server")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	select {
	case <-serverInfo.ApiServerExited.Wait():
		t.Logf("API server exited as expected")
	case <-ctx.Done():
		t.Errorf("API server did not exit within the expected time")
	}
}

func TestCanRunCleanupWithoutStoppingApiServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting(t.Name()))
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
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"CleaningResources"}`))
	req.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to start resource cleanup")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	t.Logf("Waiting for API server to complete cleanup...")
	waitErr := ctrl_testutil.WaitApiServerStatus(ctx, client, serverInfo, apiserver.ApiServerCleanupComplete)
	require.NoError(t, waitErr, "Failed to wait for API server to complete cleanup")

	t.Logf("Trying to request cleanup again (should not cause an error)...")
	// Previous request body is read and closed, need to set it again
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"CleaningResources"}`))
	resp, respErr = client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to start resource cleanup again")
	require.Equal(t, http.StatusOK, resp.StatusCode) // 200 OK not 202 Accepted
	body, bodyErr := io.ReadAll(resp.Body)
	require.NoError(t, bodyErr)
	require.JSONEq(t, `{"status":"CleanupComplete", "shutdownResourceCleanup":"Full"}`, string(body), "The server should notify the client that it already has cleaned up all resources")

	// Now try to stop the API server
	t.Logf("Stopping the API server...")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"Stopping"}`))

	resp, respErr = client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to stop the API server")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	select {
	case <-serverInfo.ApiServerExited.Wait():
		t.Logf("API server exited as expected")
	case <-ctx.Done():
		t.Errorf("API server did not exit within the expected time")
	}
}

func TestCannotCreateNewObjectsAfterClenupStarted(t *testing.T) {
	// 1. Start the API server
	// 2. Create a new Executable object (make a note that we do not expect the Executable to actually run because we have no controllers)
	// 4. Start the cleanup process and wait till it ends
	// 5. Verify the Executable object was deleted
	// 6. Try to create a new Executable object and verify the creation fails
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, testutil.NewLogForTesting(t.Name()))
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

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-cannot-create-new-objects-after-cleanup-started-successful",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "/path/to/executable-cannot-create-new-objects-after-cleanup-started-successful",
		},
	}
	t.Logf("Creating Executable object '%s'...", exe.Name)
	createErr := serverInfo.Client.Create(ctx, &exe)
	require.NoError(t, createErr, "Failed to create Executable object")

	t.Logf("Starting cleanup process...")
	req, reqCreationErr := http.NewRequestWithContext(ctx, "PATCH", adminDocUrl, nil)
	require.NoError(t, reqCreationErr)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"CleaningResources"}`))
	req.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to start resource cleanup")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	t.Logf("Waiting for API server to complete cleanup...")
	waitErr := ctrl_testutil.WaitApiServerStatus(ctx, client, serverInfo, apiserver.ApiServerCleanupComplete)
	require.NoError(t, waitErr, "Failed to wait for API server to complete cleanup")

	t.Logf("Verifying Executable object was deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &exe)

	exe = apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-cannot-create-new-objects-after-cleanup-started-expected-failure",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "/path/to/executable-cannot-create-new-objects-after-cleanup-started-expected-failure",
		},
	}
	t.Logf("Creating Executable object '%s'...", exe.Name)
	createErr = serverInfo.Client.Create(ctx, &exe)
	require.Error(t, createErr, "Executable creation should fail")
}
