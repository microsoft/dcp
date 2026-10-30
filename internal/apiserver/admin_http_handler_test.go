/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/apiserver"
	"github.com/microsoft/dcp/internal/notifications"

	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultApiServerTestTimeout       = 1 * time.Minute
	finiteRequestDrainObservationTime = 1 * time.Second
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
	runConfig := apiserver.ApiServerRunConfig{
		RequestShutdown: func(apiserver.ApiServerResourceCleanup) {},
	}
	handler := apiserver.NewAdminHttpHandler(ctx, runConfig, testutil.NewLogForTesting(t.Name()))
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

	runConfig := apiserver.ApiServerRunConfig{
		RequestShutdown: func(apiserver.ApiServerResourceCleanup) {},
	}
	handler := apiserver.NewAdminHttpHandler(ctx, runConfig, testutil.NewLogForTesting(t.Name()))
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
	runConfig := apiserver.ApiServerRunConfig{
		RequestShutdown: func(apiserver.ApiServerResourceCleanup) {
			requestShutdownCalled = true
		},
	}
	handler := apiserver.NewAdminHttpHandler(ctx, runConfig, testutil.NewLogForTesting(t.Name()))
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
		runConfig := apiserver.ApiServerRunConfig{
			RequestShutdown: func(cleanup apiserver.ApiServerResourceCleanup) {
				requestShutdownCalled = true
				cleanupPerformed = cleanup
			},
		}
		handler := apiserver.NewAdminHttpHandler(ctx, runConfig, testutil.NewLogForTesting(t.Name()+requested))

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

	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
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

// Verifies that the API server completes its stop response before exiting over HTTP/1.1 and HTTP/2 with either cleanup mode.
func TestStopApiServerCompletesResponseBeforeExit(t *testing.T) {
	for _, protocol := range []int{1, 2} {
		for _, cleanup := range []apiserver.ApiServerResourceCleanup{
			apiserver.ApiServerResourceCleanupFull,
			apiserver.ApiServerResourceCleanupNone,
		} {
			t.Run(fmt.Sprintf("HTTP%d/%s", protocol, cleanup), func(t *testing.T) {
				ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
				defer cancel()

				serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
				require.NoError(t, startupErr, "Failed to start the API server")
				defer serverInfo.Dispose()

				adminURL := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument
				patch := fmt.Sprintf(`{"status":"Stopping","shutdownResourceCleanup":%q}`, cleanup)
				request, requestErr := http.NewRequestWithContext(ctx, http.MethodPatch, adminURL, bytes.NewBufferString(patch))
				require.NoError(t, requestErr)
				request.Header.Set("Content-Type", "application/merge-patch+json")
				request.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

				client := newApiServerProtocolClient(t, serverInfo, protocol)
				response, responseErr := client.Do(request)
				require.NoError(t, responseErr, "Failed to submit request to stop the API server")
				body := readExecutionResponseBody(t, response)
				require.Equal(t, protocol, response.ProtoMajor)
				require.Equal(t, http.StatusAccepted, response.StatusCode)
				require.JSONEq(t, patch, string(body))
				requireApiServerExit(t, ctx, serverInfo)
			})
		}
	}
}

// Verifies that the API server remains alive during shutdown until an already-active finite request completes.
func TestStopApiServerWaitsForInFlightFiniteRequest(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer serverInfo.Dispose()

	client := newApiServerProtocolClient(t, serverInfo, 1)
	blockedRequestBody := []byte("not-json")
	blockedConnection, blockedReader, blockedRequest := openBlockedExecutionPatch(
		t,
		ctx,
		serverInfo,
		client,
		len(blockedRequestBody),
	)
	defer func() { _ = blockedConnection.Close() }()

	adminURL := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument
	stopRequest, stopRequestErr := http.NewRequestWithContext(ctx, http.MethodPatch, adminURL,
		bytes.NewBufferString(`{"status":"Stopping","shutdownResourceCleanup":"None"}`))
	require.NoError(t, stopRequestErr)
	stopRequest.Header.Set("Content-Type", "application/merge-patch+json")
	stopRequest.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

	stopResponse, stopResponseErr := client.Do(stopRequest)
	require.NoError(t, stopResponseErr)
	stopBody := readExecutionResponseBody(t, stopResponse)
	require.Equal(t, http.StatusAccepted, stopResponse.StatusCode)
	require.JSONEq(t, `{"status":"Stopping","shutdownResourceCleanup":"None"}`, string(stopBody))

	exitObservationTimer := time.NewTimer(finiteRequestDrainObservationTime)
	defer exitObservationTimer.Stop()
	select {
	case <-serverInfo.ApiServerExited.Wait():
		t.Fatal("API server exited while a finite request was still in flight")
	case <-exitObservationTimer.C:
	case <-ctx.Done():
		t.Fatal("test timed out while verifying that the API server remained alive")
	}

	_, bodyWriteErr := blockedConnection.Write(blockedRequestBody)
	require.NoError(t, bodyWriteErr, "Failed to complete the in-flight finite request")

	blockedResponse, blockedResponseErr := http.ReadResponse(blockedReader, blockedRequest)
	require.NoError(t, blockedResponseErr, "Failed to read the in-flight finite request response")
	blockedResponseBody, blockedResponseBodyErr := io.ReadAll(blockedResponse.Body)
	require.NoError(t, blockedResponseBodyErr)
	require.NoError(t, blockedResponse.Body.Close())
	require.Equal(t, http.StatusBadRequest, blockedResponse.StatusCode)
	require.Contains(t, string(blockedResponseBody), "could not unmarshal execution patch request")

	requireApiServerExit(t, ctx, serverInfo)
}

// Verifies that resource cleanup leaves the API server available and a subsequent stop completes its response before process exit.
func TestCleanupThenStopApiServerCompletesResponseBeforeExit(t *testing.T) {
	for _, protocol := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", protocol), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
			defer cancel()

			serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
			require.NoError(t, startupErr, "Failed to start the API server")
			defer serverInfo.Dispose()

			cleanupRequest, cleanupRequestErr := ctrl_testutil.MakeResourceCleanupRequest(ctx, serverInfo)
			require.NoError(t, cleanupRequestErr)
			client := newApiServerProtocolClient(t, serverInfo, protocol)
			cleanupResponse, cleanupResponseErr := client.Do(cleanupRequest)
			require.NoError(t, cleanupResponseErr, "Failed to submit request to start resource cleanup")
			readExecutionResponseBody(t, cleanupResponse)
			require.Equal(t, protocol, cleanupResponse.ProtoMajor)
			require.Equal(t, http.StatusAccepted, cleanupResponse.StatusCode)

			waitErr := ctrl_testutil.WaitApiServerStatus(ctx, client, serverInfo, apiserver.ApiServerCleanupComplete)
			require.NoError(t, waitErr, "Failed to wait for API server to complete cleanup")

			repeatRequest, repeatRequestErr := ctrl_testutil.MakeResourceCleanupRequest(ctx, serverInfo)
			require.NoError(t, repeatRequestErr)
			repeatResponse, repeatResponseErr := client.Do(repeatRequest)
			require.NoError(t, repeatResponseErr, "Failed to submit resource cleanup again")
			repeatBody := readExecutionResponseBody(t, repeatResponse)
			require.Equal(t, http.StatusOK, repeatResponse.StatusCode)
			require.JSONEq(t, `{"status":"CleanupComplete","shutdownResourceCleanup":"Full"}`, string(repeatBody))

			stopRequest, stopRequestErr := http.NewRequestWithContext(ctx, http.MethodPatch, cleanupRequest.URL.String(),
				bytes.NewBufferString(`{"status":"Stopping"}`))
			require.NoError(t, stopRequestErr)
			stopRequest.Header = cleanupRequest.Header.Clone()
			stopResponse, stopResponseErr := client.Do(stopRequest)
			require.NoError(t, stopResponseErr, "Failed to submit request to stop the API server")
			stopBody := readExecutionResponseBody(t, stopResponse)
			require.Equal(t, protocol, stopResponse.ProtoMajor)
			require.Equal(t, http.StatusAccepted, stopResponse.StatusCode)
			require.JSONEq(t, `{"status":"Stopping","shutdownResourceCleanup":"Full"}`, string(stopBody))
			requireApiServerExit(t, ctx, serverInfo)
		})
	}
}

// Verifies that shutdown ends open watches and both active and pending follow-log streams without client cancellation.
func TestStopApiServerCancelsLongRunningRequests(t *testing.T) {
	for _, protocol := range []int{1, 2} {
		for _, stream := range []string{"watch", "logs-ready", "logs-pending"} {
			t.Run(fmt.Sprintf("HTTP%d/%s", protocol, stream), func(t *testing.T) {
				ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
				defer cancel()
				serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
				require.NoError(t, startupErr)
				defer serverInfo.Dispose()
				client := newApiServerProtocolClient(t, serverInfo, protocol)

				logDirectory := t.TempDir()
				executable := &apiv1.Executable{
					ObjectMeta: metav1.ObjectMeta{Name: "shutdown-stream"},
					Spec:       apiv1.ExecutableSpec{ExecutablePath: filepath.Join(logDirectory, "unused")},
				}
				require.NoError(t, serverInfo.Client.Create(ctx, executable))
				streamURL := serverInfo.ClientConfig.Host + "/apis/" + apiv1.GroupVersion.String() + "/executables?watch=true"
				if stream != "watch" {
					executable.Status.State = apiv1.ExecutableStateRunning
					if stream == "logs-ready" {
						logPath := filepath.Join(logDirectory, "stdout.log")
						require.NoError(t, usvc_io.WriteFile(logPath, []byte("ready\n"), 0o600))
						executable.Status.StdOutFile = logPath
					}
					require.NoError(t, serverInfo.Client.Status().Update(ctx, executable))
					streamURL = serverInfo.ClientConfig.Host + "/apis/" + apiv1.GroupVersion.String() + "/executables/" +
						executable.Name + "/" + apiv1.LogSubresourceName + "?follow=true"
				}

				streamRequest, streamRequestErr := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
				require.NoError(t, streamRequestErr)
				streamRequest.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)
				streamResponse, streamResponseErr := client.Do(streamRequest)
				require.NoError(t, streamResponseErr)
				defer func() { require.NoError(t, streamResponse.Body.Close()) }()
				require.Equal(t, protocol, streamResponse.ProtoMajor)
				require.Equal(t, http.StatusOK, streamResponse.StatusCode)
				streamReader := bufio.NewReader(streamResponse.Body)
				if stream != "logs-pending" {
					firstLine, firstLineErr := streamReader.ReadString('\n')
					require.NoError(t, firstLineErr)
					if stream == "watch" {
						var event struct {
							Type string `json:"type"`
						}
						require.NoError(t, json.Unmarshal([]byte(firstLine), &event))
						require.Equal(t, "ADDED", event.Type)
					} else {
						require.Equal(t, "ready\n", firstLine)
					}
				}

				streamDone := make(chan error, 1)
				go func() {
					_, streamReadErr := io.Copy(io.Discard, streamReader)
					streamDone <- streamReadErr
				}()

				adminURL := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument
				stopRequest, stopRequestErr := http.NewRequestWithContext(ctx, http.MethodPatch, adminURL,
					bytes.NewBufferString(`{"status":"Stopping","shutdownResourceCleanup":"None"}`))
				require.NoError(t, stopRequestErr)
				stopRequest.Header.Set("Content-Type", "application/merge-patch+json")
				stopRequest.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)
				stopResponse, stopResponseErr := client.Do(stopRequest)
				require.NoError(t, stopResponseErr)
				stopBody := readExecutionResponseBody(t, stopResponse)
				require.Equal(t, http.StatusAccepted, stopResponse.StatusCode)
				require.JSONEq(t, `{"status":"Stopping","shutdownResourceCleanup":"None"}`, string(stopBody))

				select {
				case streamErr, channelOpen := <-streamDone:
					require.True(t, channelOpen)
					require.NoError(t, streamErr)
				case <-ctx.Done():
					t.Fatal("long-running request did not end after API server shutdown")
				}
				require.NoError(t, ctx.Err())
				requireApiServerExit(t, ctx, serverInfo)
			})
		}
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

	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
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

func TestCanCreateV2Namespace(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer func() {
		serverInfo.Dispose()

		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
		}
	}()

	ns := apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-v2-namespace",
		},
	}
	createErr := serverInfo.Client.Create(ctx, &ns)
	require.NoError(t, createErr, "Failed to create V2 Namespace")

	var got apiv2.Namespace
	getErr := serverInfo.Client.Get(ctx, types.NamespacedName{Name: ns.Name}, &got)
	require.NoError(t, getErr, "Failed to get V2 Namespace")
	require.Equal(t, ns.Name, got.Name)
	require.Empty(t, got.Namespace)

	var list apiv2.NamespaceList
	listErr := serverInfo.Client.List(ctx, &list)
	require.NoError(t, listErr, "Failed to list V2 Namespaces")
	require.Len(t, list.Items, 1)
	require.Equal(t, ns.Name, list.Items[0].Name)
}

func TestCanCreateV2PhysicalContainer(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer func() {
		serverInfo.Dispose()

		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
		}
	}()

	ns := apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-v2-namespace",
		},
	}
	createErr := serverInfo.Client.Create(ctx, &ns)
	require.NoError(t, createErr, "Failed to create V2 Namespace")

	pci := apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-v2-physical-container-image",
			Namespace: ns.Name,
		},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "test-image"}},
	}
	createErr = serverInfo.Client.Create(ctx, &pci)
	require.NoError(t, createErr, "Failed to create V2 PhysicalContainerImage")

	pc := apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-v2-physical-container",
			Namespace: ns.Name,
		},
		Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{ImageRef: pci.Name}},
	}
	createErr = serverInfo.Client.Create(ctx, &pc)
	require.NoError(t, createErr, "Failed to create V2 PhysicalContainer")

	var got apiv2.PhysicalContainer
	getErr := serverInfo.Client.Get(ctx, pc.NamespacedName(), &got)
	require.NoError(t, getErr, "Failed to get V2 PhysicalContainer")
	require.Equal(t, pc.Name, got.Name)
	require.Equal(t, ns.Name, got.Namespace)
	require.Equal(t, pci.Name, got.Spec.Container.ImageRef)

	var list apiv2.PhysicalContainerList
	listErr := serverInfo.Client.List(ctx, &list, ctrl_client.InNamespace(ns.Name))
	require.NoError(t, listErr, "Failed to list V2 PhysicalContainers")
	require.Len(t, list.Items, 1)
	require.Equal(t, pc.Name, list.Items[0].Name)

	pid := int64(os.Getpid())
	pp := apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-v2-physical-process",
			Namespace: ns.Name,
		},
		Spec: apiv2.PhysicalProcessSpec{PID: &pid},
	}
	createErr = serverInfo.Client.Create(ctx, &pp)
	require.NoError(t, createErr, "Failed to create V2 PhysicalProcess")

	var gotProcess apiv2.PhysicalProcess
	getErr = serverInfo.Client.Get(ctx, pp.NamespacedName(), &gotProcess)
	require.NoError(t, getErr, "Failed to get V2 PhysicalProcess")
	require.Equal(t, pp.Name, gotProcess.Name)
	require.Equal(t, pid, *gotProcess.Spec.PID)

	var processList apiv2.PhysicalProcessList
	listErr = serverInfo.Client.List(ctx, &processList, ctrl_client.InNamespace(ns.Name))
	require.NoError(t, listErr, "Failed to list V2 PhysicalProcesses")
	require.Len(t, processList.Items, 1)
	require.Equal(t, pp.Name, processList.Items[0].Name)
}

func TestCanCapturePerfTrace(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()

	perfTraceCaptured := false
	receivedPerfTraceNotification := false
	var onNotification notifications.NotifySubscribersFunc = func(n notifications.Notification) error {
		if n.Kind() == notifications.NotificationKindPerftraceRequest {
			receivedPerfTraceNotification = true
		}
		return nil
	}

	runConfig := apiserver.ApiServerRunConfig{
		RequestShutdown:    func(apiserver.ApiServerResourceCleanup) {},
		NotificationSource: onNotification,
		CollectPerfTrace: func(context.Context, context.CancelFunc, logr.Logger) error {
			perfTraceCaptured = true
			return nil
		},
	}

	handler := apiserver.NewAdminHttpHandler(ctx, runConfig, testutil.NewLogForTesting(t.Name()))
	var req *http.Request
	var w *httptest.ResponseRecorder
	var resp *http.Response

	req = httptest.NewRequestWithContext(ctx, "PUT", apiserver.AdminPathPrefix+apiserver.PerfTraceDocument+"?duration=10s", nil)
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp = w.Result()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.True(t, perfTraceCaptured)
	require.True(t, receivedPerfTraceNotification)
}

func newApiServerProtocolClient(t *testing.T, serverInfo *ctrl_testutil.ApiServerInfo, protocol int) *http.Client {
	t.Helper()
	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	transport, isHTTPTransport := client.Transport.(*http.Transport)
	require.True(t, isHTTPTransport)
	transport.ForceAttemptHTTP2 = protocol == 2
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func openBlockedExecutionPatch(
	t *testing.T,
	ctx context.Context,
	serverInfo *ctrl_testutil.ApiServerInfo,
	client *http.Client,
	contentLength int,
) (*tls.Conn, *bufio.Reader, *http.Request) {
	t.Helper()

	serverURL, parseErr := url.Parse(serverInfo.ClientConfig.Host)
	require.NoError(t, parseErr)
	transport, isHTTPTransport := client.Transport.(*http.Transport)
	require.True(t, isHTTPTransport)
	tlsConfig := transport.TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"http/1.1"}
	tlsConfig.ServerName = serverURL.Hostname()

	dialer := &net.Dialer{}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		dialer.Deadline = deadline
	}
	connection, connectionErr := tls.DialWithDialer(dialer, "tcp", serverURL.Host, tlsConfig)
	require.NoError(t, connectionErr)
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		require.NoError(t, connection.SetDeadline(deadline))
	}

	requestPath := apiserver.AdminPathPrefix + apiserver.ExecutionDocument
	_, headerWriteErr := fmt.Fprintf(
		connection,
		"PATCH %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/merge-patch+json\r\nContent-Length: %d\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n",
		requestPath,
		serverURL.Host,
		serverInfo.ClientConfig.BearerToken,
		contentLength,
	)
	require.NoError(t, headerWriteErr)

	reader := bufio.NewReader(connection)
	statusLine, statusLineErr := reader.ReadString('\n')
	require.NoError(t, statusLineErr)
	require.Equal(t, "HTTP/1.1 100 Continue\r\n", statusLine)
	for {
		headerLine, headerLineErr := reader.ReadString('\n')
		require.NoError(t, headerLineErr)
		if headerLine == "\r\n" {
			break
		}
	}

	request := &http.Request{Method: http.MethodPatch}
	return connection, reader, request
}

func readExecutionResponseBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, bodyErr := io.ReadAll(response.Body)
	require.NoError(t, bodyErr, "API server response did not complete")
	return body
}

func requireApiServerExit(t *testing.T, ctx context.Context, serverInfo *ctrl_testutil.ApiServerInfo) {
	t.Helper()
	select {
	case <-serverInfo.ApiServerExited.Wait():
	case <-ctx.Done():
		t.Fatal("API server did not exit after completing its stop response")
	}
}

func createApiServerForHttpHandlerTests(testName string, ctx context.Context) (*ctrl_testutil.ApiServerInfo, error) {
	sessionFolder, sessionFolderErr := testutil.CreateTestSessionDir()
	if sessionFolderErr != nil {
		return nil, fmt.Errorf("failed to create session folder for API server instance: %w", sessionFolderErr)
	}

	log := testutil.NewLogWithResourceSinkForTesting(testName, sessionFolder)

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone, log, sessionFolder)
	return serverInfo, startupErr
}
