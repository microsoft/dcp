/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	serverfilters "k8s.io/apiserver/pkg/server/filters"
	clientrest "k8s.io/client-go/rest"
	clientwebsocket "k8s.io/client-go/transport/websocket"
	"k8s.io/streaming/pkg/httpstream"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/pkg/generated/openapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

// duplexProbeStorage exercises the Connecter extension point without introducing a debug API or adapter execution.
type duplexProbeStorage struct {
	parent         rest.Getter
	disconnected   chan error
	setupDelay     time.Duration
	setupCancelled chan error
}

func (*duplexProbeStorage) New() runtime.Object                               { return &apiv2.Project{} }
func (*duplexProbeStorage) Destroy()                                          {}
func (*duplexProbeStorage) NamespaceScoped() bool                             { return true }
func (*duplexProbeStorage) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }
func (*duplexProbeStorage) ConnectMethods() []string                          { return []string{http.MethodGet} }

func (storage *duplexProbeStorage) Connect(ctx context.Context, name string, _ runtime.Object, _ rest.Responder) (http.Handler, error) {
	if _, parentErr := storage.parent.Get(ctx, name, &metav1.GetOptions{}); parentErr != nil {
		return nil, parentErr
	}
	if storage.setupDelay > 0 {
		timer := time.NewTimer(storage.setupDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			storage.setupCancelled <- ctx.Err()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, apierrors.NewTimeoutError("probe setup exceeded request deadline", 0)
			}
			return nil, ctx.Err()
		}
	}
	namespace := requestinfo.NamespaceValue(ctx)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{Subprotocols: []string{"dap"}}
		connection, upgradeErr := upgrader.Upgrade(writer, request, nil)
		if upgradeErr != nil {
			storage.disconnected <- upgradeErr
			return
		}
		defer connection.Close()
		greeting := []byte(fmt.Sprintf(`{"type":"event","event":"probe","body":{"namespace":%q,"name":%q}}`, namespace, name))
		frame := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(greeting), greeting))
		if writeErr := connection.WriteMessage(websocket.BinaryMessage, frame); writeErr != nil {
			storage.disconnected <- writeErr
			return
		}
		for {
			messageType, payload, readErr := connection.ReadMessage()
			if readErr != nil {
				if websocket.IsCloseError(readErr, websocket.CloseNormalClosure) {
					storage.disconnected <- nil
				} else {
					storage.disconnected <- readErr
				}
				return
			}
			if writeErr := connection.WriteMessage(messageType, payload); writeErr != nil {
				storage.disconnected <- writeErr
				return
			}
		}
	}), nil
}

// Verifies that tilt-apiserver accepts a namespaced Connecter subresource through its authenticated
// TLS handler chain, permits pre-upgrade setup and bidirectional traffic beyond the ordinary timeout,
// and retains normal timeouts on unclassified connectors without an application handshake.
func TestDuplexSubresourceWebSocketTransport(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	log := testutil.NewLogForTesting(t.Name())
	probe := &duplexProbeStorage{
		disconnected: make(chan error, 1), setupDelay: 500 * time.Millisecond,
		setupCancelled: make(chan error, 1),
	}
	parent := &apiv2.Project{}
	serverBuilder := builder.NewServerBuilder().
		WithResourceMemoryStorage(parent, "data").
		WithOpenAPIDefinitions("duplex-probe", "1", openapi.GetOpenAPIDefinitions).
		WithBearerToken("duplex-probe-test-token")
	for _, additional := range apiv2.AdditionalTypes {
		serverBuilder.WithResource(additional)
	}
	provider := func(_ *runtime.Scheme, _ generic.RESTOptionsGetter) (rest.Storage, error) {
		return probe, nil
	}
	serverBuilder.WithSubResourceAndHandler(parent, "duplexprobe", tiltapiserver.StorageProvider(provider))
	serverBuilder.WithSubResourceAndHandler(parent, "duplextimeout", tiltapiserver.StorageProvider(provider))
	options, optionsErr := serverBuilder.ToServerOptions()
	require.NoError(t, optionsErr)
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	t.Cleanup(func() { _ = listener.Close() })
	options.ServingOptions.Listener = listener
	options.ServingOptions.BindAddress = net.ParseIP("127.0.0.1")
	options.ServingOptions.BindPort = listener.Addr().(*net.TCPAddr).Port
	config, configErr := options.Config()
	require.NoError(t, configErr)
	require.NoError(t, configureForLogServing(config, log))
	// A deliberately short timeout demonstrates that long-running classification is required.
	config.GenericConfig.RequestTimeout = 250 * time.Millisecond
	config.GenericConfig.LongRunningFunc = serverfilters.BasicLongRunningRequestCheck(
		sets.NewString("watch"), sets.NewString(apiv2.LogSubresourceName, "duplexprobe"),
	)
	parentStorage, parentStorageErr := config.ExtraConfig.APIs[parent.GetGroupVersionResource()](config.ExtraConfig.Scheme, config.GenericConfig.RESTOptionsGetter)
	require.NoError(t, parentStorageErr)
	probe.parent = parentStorage.(rest.Getter)
	// Serve the actual installed API/filter chain, rather than a directly registered echo handler.
	stopped, startErr := runServerFromCompletedConfig(config.Complete(), ctx, ApiServerRunConfig{
		RequestShutdown: func(ApiServerResourceCleanup) { cancel() },
	}, log)
	require.NoError(t, startErr)
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "duplex"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}}}}
	clientConfig := clientrest.CopyConfig(config.GenericConfig.LoopbackClientConfig)
	apiClient, clientErr := ctrlclient.New(clientConfig, ctrlclient.Options{Scheme: dcpclient.NewScheme()})
	require.NoError(t, clientErr)
	require.NoError(t, apiClient.Create(ctx, project))
	location := clientConfig.Host + "/apis/" + apiv2.GroupVersion.String() + "/namespaces/duplex/projects/probe/duplexprobe"
	for _, testCase := range []struct {
		name     string
		token    string
		location string
	}{
		{name: "invalid credentials", token: "invalid-probe-token", location: location},
		{name: "missing parent", token: clientConfig.BearerToken, location: strings.Replace(location, "/projects/probe/", "/projects/missing/", 1)},
		{name: "ordinary request timeout", token: clientConfig.BearerToken, location: strings.Replace(location, "/duplexprobe", "/duplextimeout", 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rejectedConfig := clientrest.CopyConfig(clientConfig)
			rejectedConfig.BearerToken = testCase.token
			rejectedTransport, rejectedHolder, rejectedTransportErr := clientwebsocket.RoundTripperFor(rejectedConfig)
			require.NoError(t, rejectedTransportErr)
			rejectedRequest, rejectedRequestErr := http.NewRequestWithContext(ctx, http.MethodGet, testCase.location, nil)
			require.NoError(t, rejectedRequestErr)
			rejectedConnection, rejectionErr := clientwebsocket.Negotiate(rejectedTransport, rejectedHolder, rejectedRequest, "dap")
			if rejectedConnection != nil {
				require.NoError(t, rejectedConnection.Close())
			}
			require.Error(t, rejectionErr)
			if testCase.name == "ordinary request timeout" {
				select {
				case setupErr := <-probe.setupCancelled:
					require.ErrorIs(t, setupErr, context.DeadlineExceeded)
				case <-ctx.Done():
					t.Fatal("ordinary request deadline did not cancel connector setup")
				}
				// The timeout filter can abort the connection while the cancelled handler returns its error.
				if errors.Is(rejectionErr, io.EOF) {
					return
				}
			}
			var upgradeFailure *httpstream.UpgradeFailureError
			require.True(t, errors.As(rejectionErr, &upgradeFailure), "%v", rejectionErr)
			if testCase.name == "missing parent" {
				require.True(t, apierrors.IsNotFound(upgradeFailure.Cause), "%v", rejectionErr)
			} else if testCase.name == "ordinary request timeout" {
				require.True(t, apierrors.IsTimeout(upgradeFailure.Cause), "%v", rejectionErr)
			} else {
				require.True(t, apierrors.IsUnauthorized(upgradeFailure.Cause) || apierrors.IsForbidden(upgradeFailure.Cause), "%v", rejectionErr)
			}
		})
	}
	roundTripper, connectionHolder, transportErr := clientwebsocket.RoundTripperFor(clientConfig)
	require.NoError(t, transportErr)
	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	require.NoError(t, requestErr)
	connection, connectErr := clientwebsocket.Negotiate(roundTripper, connectionHolder, request, "dap")
	require.NoError(t, connectErr)
	defer connection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetReadDeadline(deadline))
	_, greeting, greetingErr := connection.ReadMessage()
	require.NoError(t, greetingErr)
	require.Contains(t, string(greeting), `"namespace":"duplex","name":"probe"`)
	require.True(t, strings.HasPrefix(string(greeting), "Content-Length: "))
	timer := time.NewTimer(2 * config.GenericConfig.RequestTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("test context expired before crossing the request timeout")
	}
	for _, body := range []string{
		`{"seq":1,"type":"request","command":"initialize"}`,
		`{"seq":2,"type":"request","command":"attach"}`,
	} {
		payload := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
		require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(payload)))
		_, response, responseErr := connection.ReadMessage()
		require.NoError(t, responseErr)
		require.Equal(t, payload, string(response))
	}
	require.NoError(t, connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
	select {
	case disconnectErr := <-probe.disconnected:
		require.NoError(t, disconnectErr)
	case <-ctx.Done():
		t.Fatal("server did not observe client disconnect")
	}
}

var _ rest.Storage = (*duplexProbeStorage)(nil)
var _ rest.Connecter = (*duplexProbeStorage)(nil)
var _ rest.Scoper = (*duplexProbeStorage)(nil)
