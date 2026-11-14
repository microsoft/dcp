/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	auditpolicy "k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	auditlog "k8s.io/apiserver/plugin/pkg/audit/log"
	clientrest "k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/kubeconfig"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type debugLookupFaultStorage struct {
	rest.StandardStorage
}

func (storage *debugLookupFaultStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	switch name {
	case "panic-debug-log-probe":
		panic("injected debug lookup panic")
	case "abort-debug-log-probe":
		panic(http.ErrAbortHandler)
	case "failure-debug-log-probe":
		return nil, apierrors.NewInternalError(fmt.Errorf("injected debug lookup failure"))
	default:
		return storage.StandardStorage.Get(ctx, name, options)
	}
}

func registerDebugServingTestStorage(config *tiltapiserver.Config, ctx context.Context, writer *projectDebugStatusWriter, log logr.Logger) {
	gvr := (&apiv2.Project{}).GetGroupVersionResource()
	parentProvider := config.ExtraConfig.APIs[gvr]
	namespaceProvider := config.ExtraConfig.APIs[(&apiv2.Namespace{}).GetGroupVersionResource()]
	gvr.Resource += "/" + apiv2.DebugSubresourceName
	config.ExtraConfig.APIs[gvr] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		parent, parentErr := parentProvider(scheme, getter)
		if parentErr != nil {
			return nil, parentErr
		}
		namespaces, namespaceErr := namespaceProvider(scheme, getter)
		if namespaceErr != nil {
			return nil, namespaceErr
		}
		return newProjectDebugStorage(ctx, parent.(rest.StandardStorage), namespaces.(rest.Getter),
			writer, process.NewOSExecutor(log), log), nil
	}
}

// Verifies that debug credentials stay out of verbose server and audit logs during admission,
// missing-resource lookup, lookup errors/panics/aborts, failed adapter launch, and successful serving.
func TestProjectDebugTokensDoNotAppearInServerLogs(t *testing.T) {
	const token = "debug-session-credential-log-probe"
	const wrongToken = "rejected-debug-credential-log-probe"
	t.Setenv(kubeconfig.DCP_SECURE_TOKEN, "")
	t.Setenv(DCP_DEBUG_SESSION_TOKEN, token)
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	flags := flag.NewFlagSet("debug-log-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	require.NoError(t, flags.Set("v", "9"))
	logs, auditLogs, stderrLogs := testutil.NewThreadSafeBuffer(), testutil.NewThreadSafeBuffer(), testutil.NewThreadSafeBuffer()
	originalLogOutput := stdlog.Writer()
	stdlog.SetOutput(stderrLogs)
	t.Cleanup(func() { stdlog.SetOutput(originalLogOutput) })
	log := zapr.NewLogger(zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(logs), zapcore.Level(-10))))
	klog.SetLogger(log)
	var requests sync.WaitGroup
	var bearerToken string
	t.Cleanup(func() {
		requests.Wait()
		serverOutput, readErr := io.ReadAll(logs)
		require.NoError(t, readErr)
		auditOutput, auditReadErr := io.ReadAll(auditLogs)
		require.NoError(t, auditReadErr)
		stderrOutput, stderrReadErr := io.ReadAll(stderrLogs)
		require.NoError(t, stderrReadErr)
		require.Contains(t, string(serverOutput), `"msg":"HTTP"`)
		require.Contains(t, string(serverOutput), "missing-debug-log-probe")
		require.Contains(t, string(serverOutput), `"hijacked":true`)
		require.Contains(t, string(serverOutput), "apiserver panic'd")
		require.Contains(t, string(serverOutput), "Timeout or abort while handling")
		require.Contains(t, string(auditOutput), `"kind":"Event"`)
		require.Contains(t, string(auditOutput), "missing-debug-log-probe")
		require.Contains(t, string(stderrOutput), "http: panic serving")
		for _, credential := range []string{token, wrongToken, bearerToken} {
			require.NotEmpty(t, credential)
			require.NotContains(t, string(serverOutput), credential)
			require.NotContains(t, string(auditOutput), credential)
			require.NotContains(t, string(stderrOutput), credential)
		}
	})
	ctx, cancel := testutil.GetTestContext(t, 0)
	t.Cleanup(cancel)
	dcppaths.EnableTestPathProbing()
	var config *clientrest.Config
	apiClient, _ := startProjectDebugStatusServer(t, ctx, func(serverConfig *tiltapiserver.Config, writer *projectDebugStatusWriter) {
		config = clientrest.CopyConfig(serverConfig.GenericConfig.LoopbackClientConfig)
		bearerToken = config.BearerToken
		originalChain := serverConfig.GenericConfig.BuildHandlerChainFunc
		serverConfig.GenericConfig.BuildHandlerChainFunc = func(handler http.Handler, genericConfig *kubeapiserver.Config) http.Handler {
			chain := originalChain(handler, genericConfig)
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				defer requests.Done()
				chain.ServeHTTP(writer, request)
			})
		}
		serverConfig.GenericConfig.AuditBackend = auditlog.NewBackend(auditLogs, auditlog.FormatJson, auditv1.SchemeGroupVersion)
		serverConfig.GenericConfig.AuditPolicyRuleEvaluator = auditpolicy.NewPolicyRuleEvaluator(&auditinternal.Policy{
			Rules: []auditinternal.PolicyRule{{Level: auditinternal.LevelRequestResponse}},
		})
		registerDebugServingTestStorage(serverConfig, ctx, writer, log)
		gvr := (&apiv2.Project{}).GetGroupVersionResource()
		gvr.Resource += "/" + apiv2.DebugSubresourceName
		debugProvider := serverConfig.ExtraConfig.APIs[gvr]
		serverConfig.ExtraConfig.APIs[gvr] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
			storage, storageErr := debugProvider(scheme, getter)
			if storageErr != nil {
				return nil, storageErr
			}
			debugStorage := storage.(*projectDebugStorage)
			debugStorage.parent = &debugLookupFaultStorage{StandardStorage: debugStorage.parent}
			return storage, nil
		}
	})
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	successful := project.DeepCopy()
	successful.Name, successful.ResourceVersion, successful.UID = "successful-debug-log-probe", "", ""
	successful.Spec.DebugAdapter.Args = []string{tool, "--dap-stdio"}
	successful.Status = apiv2.ProjectStatus{}
	require.NoError(t, apiClient.Create(ctx, successful))
	successful.Status = project.Status
	require.NoError(t, apiClient.Status().Update(ctx, successful))
	tlsConfig, tlsErr := clientrest.TLSConfigFor(config)
	require.NoError(t, tlsErr)
	t.Run("invalid-upgrade", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: tlsConfig}
		defer transport.CloseIdleConnections()
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
			config.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/debug/projects/"+project.Name+"/debug", nil)
		require.NoError(t, requestErr)
		request.Header.Set("Authorization", "Bearer "+bearerToken)
		request.Header.Set("X-DCP-Debug-Session-Token", token)
		response, responseErr := (&http.Client{Transport: transport}).Do(request)
		require.NoError(t, responseErr)
		defer response.Body.Close()
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NotContains(t, string(body), token)
	})
	dialer := &websocket.Dialer{TLSClientConfig: tlsConfig, Subprotocols: []string{"dap"}}
	for _, scenario := range []struct {
		name   string
		target string
		token  string
		bearer string
		status int
	}{
		{name: "missing", target: "missing-debug-log-probe", token: token, bearer: bearerToken, status: http.StatusNotFound},
		{name: "wrong-token", target: project.Name, token: wrongToken, bearer: bearerToken, status: http.StatusForbidden},
		{name: "missing-token", target: project.Name, bearer: bearerToken, status: http.StatusForbidden},
		{name: "invalid-bearer", target: project.Name, token: token, bearer: "invalid-log-probe-bearer", status: http.StatusForbidden},
		{name: "lookup-failure", target: "failure-debug-log-probe", token: token, bearer: bearerToken, status: http.StatusInternalServerError},
		{name: "lookup-panic", target: "panic-debug-log-probe", token: token, bearer: bearerToken},
		{name: "lookup-abort", target: "abort-debug-log-probe", token: token, bearer: bearerToken},
		{name: "launch-failure", target: project.Name, token: token, bearer: bearerToken, status: http.StatusInternalServerError},
		{name: "successful", target: successful.Name, token: token, bearer: bearerToken, status: http.StatusSwitchingProtocols},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			headers := http.Header{"Authorization": []string{"Bearer " + scenario.bearer}}
			if scenario.token != "" {
				headers.Set("X-DCP-Debug-Session-Token", scenario.token)
			}
			location := strings.Replace(config.Host, "https://", "wss://", 1) +
				"/apis/" + apiv2.GroupVersion.String() + "/namespaces/debug/projects/" + scenario.target + "/debug"
			connection, response, connectErr := dialer.DialContext(ctx, location, headers)
			if response != nil {
				defer response.Body.Close()
			}
			if scenario.status == 0 {
				require.Error(t, connectErr)
				require.Nil(t, response)
				return
			}
			require.NotNil(t, response)
			require.Equal(t, scenario.status, response.StatusCode)
			if scenario.status == http.StatusSwitchingProtocols {
				require.NoError(t, connectErr)
				require.NoError(t, connection.Close())
			} else {
				require.Error(t, connectErr)
				body, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.NotContains(t, string(body), token)
				require.NotContains(t, string(body), wrongToken)
			}
		})
	}
}
