/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgorest "k8s.io/client-go/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that cancellation leaves the owned API server available until its teardown owner disposes it.
func TestApiServerCancellationPreservesServerUntilDispose(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	sessionFolder, folderErr := testutil.CreateTestSessionDir()
	require.NoError(t, folderErr)
	core, recorded := observer.New(zapcore.DebugLevel)
	log := zapr.NewLogger(zap.New(core))
	server, startErr := StartApiServer(serverCtx, ApiServerFlagsNone, log, sessionFolder)
	require.NoError(t, startErr)
	defer server.Dispose()
	t.Logf("Owned API server PID: %d", server.ApiServerPID)
	cancelServer()
	// The controller manager may still be joining its workers before Dispose is called.
	// Its API server must remain available throughout that work.
	requestErr := server.RestClient.Get().AbsPath("/apis").Do(ctx).Error()
	require.NoError(t, requestErr)
	require.False(t, server.ApiServerExited.Frozen())
	server.Dispose()
	require.True(t, server.ApiServerExited.Frozen())
	require.True(t, server.ApiServerDisposalComplete.Frozen())
	require.Empty(t, recorded.FilterLevelExact(zapcore.ErrorLevel).All())
	for _, entry := range recorded.All() {
		t.Logf("%s %s %v", entry.Level, entry.Message, entry.ContextMap())
	}
}

// Verifies that disposal reports a failed shutdown request and joins the owned process after cancellation.
func TestApiServerDisposeReportsShutdownFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	sessionFolder, folderErr := testutil.CreateTestSessionDir()
	require.NoError(t, folderErr)
	core, recorded := observer.New(zapcore.DebugLevel)
	log := zapr.NewLogger(zap.New(core))
	server, startErr := StartApiServer(ctx, ApiServerFlagsNone, log, sessionFolder)
	require.NoError(t, startErr)
	defer server.Dispose()
	t.Logf("Owned API server PID: %d", server.ApiServerPID)
	shutdownFailure := errors.New("injected shutdown transport failure")
	config := clientgorest.CopyConfig(server.ClientConfig)
	config.GroupVersion = &apiv1.GroupVersion
	config.NegotiatedSerializer = serializer.NewCodecFactory(dcpclient.NewScheme())
	config.APIPath = "/apis"
	config.WrapTransport = func(_ http.RoundTripper) http.RoundTripper {
		return shutdownFailureTransport{err: shutdownFailure}
	}
	failingClient, clientErr := clientgorest.RESTClientFor(config)
	require.NoError(t, clientErr)
	server.RestClient = failingClient
	server.Dispose()
	require.True(t, server.ApiServerExited.Frozen())
	require.True(t, server.ApiServerDisposalComplete.Frozen())
	failures := recorded.FilterMessage("Failed to request API server shutdown").All()
	require.Len(t, failures, 1)
	require.Contains(t, failures[0].ContextMap()["error"], shutdownFailure.Error())
	for _, entry := range recorded.All() {
		t.Logf("%s %s %v", entry.Level, entry.Message, entry.ContextMap())
	}
}

type shutdownFailureTransport struct {
	err error
}

func (transport shutdownFailureTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, transport.err
}
