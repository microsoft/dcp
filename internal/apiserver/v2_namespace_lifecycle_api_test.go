/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver_test

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgorest "k8s.io/client-go/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2NamespaceLifecycleAPIDryRunOptions(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()
	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr)
	defer serverInfo.Dispose()
	namespacesPath := "/apis/" + apiv2.GroupVersion.String() + "/namespaces"

	createBody := []byte(fmt.Sprintf(`{"apiVersion":%q,"kind":"Namespace","metadata":{"name":"dry-create"}}`, apiv2.GroupVersion.String()))
	createErr := serverInfo.RestClient.Post().AbsPath(namespacesPath).
		Param("dryRun", metav1.DryRunAll).SetHeader("Content-Type", "application/json").
		Body(createBody).Do(ctx).Error()
	require.True(t, apierrors.IsBadRequest(createErr), "%v", createErr)
	getCreatedErr := serverInfo.Client.Get(ctx, types.NamespacedName{Name: "dry-create"}, &apiv2.Namespace{})
	require.True(t, apierrors.IsNotFound(getCreatedErr))

	testCases := []struct {
		name    string
		query   string
		body    string
		deleted bool
	}{
		{name: "query-only", query: metav1.DryRunAll},
		{name: "body-only", body: `{"dryRun":["All"]}`},
		{name: "empty-body-options", query: metav1.DryRunAll, body: `{}`, deleted: true},
		{name: "empty-body-dry-run", query: metav1.DryRunAll, body: `{"dryRun":[]}`, deleted: true},
		{name: "both", query: metav1.DryRunAll, body: `{"dryRun":["All"]}`},
		{name: "ignored-invalid-query", query: "invalid", body: `{}`, deleted: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testCase.name}}
			require.NoError(t, serverInfo.Client.Create(ctx, namespace))
			deleteRequest := serverInfo.RestClient.Delete().AbsPath(namespacesPath + "/" + namespace.Name)
			if testCase.query != "" {
				deleteRequest.Param("dryRun", testCase.query)
			}
			if testCase.body != "" {
				deleteRequest.SetHeader("Content-Type", "application/json").Body([]byte(testCase.body))
			}
			deleteErr := deleteRequest.Do(ctx).Error()
			if testCase.deleted {
				require.NoError(t, deleteErr)
			} else {
				require.True(t, apierrors.IsBadRequest(deleteErr), "%v", deleteErr)
			}
			stored := &apiv2.Namespace{}
			require.NoError(t, serverInfo.Client.Get(ctx, namespace.NamespacedName(), stored))
			if testCase.deleted {
				require.NotNil(t, stored.DeletionTimestamp)
			} else {
				require.Nil(t, stored.DeletionTimestamp)
				require.Equal(t, namespace.ResourceVersion, stored.ResourceVersion)
			}
			pid := int64(2147483647)
			child := &apiv2.PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: namespace.Name},
				Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
			}
			childErr := serverInfo.Client.Create(ctx, child)
			if testCase.deleted {
				require.True(t, apierrors.IsForbidden(childErr), "%v", childErr)
			} else {
				require.NoError(t, childErr)
			}
		})
	}
	collectionErr := serverInfo.RestClient.Delete().AbsPath(namespacesPath).
		SetHeader("Content-Type", "application/json").Body([]byte(`{}`)).Do(ctx).Error()
	require.True(t, apierrors.IsMethodNotSupported(collectionErr), "%v", collectionErr)
}

func TestV2NamespaceLifecycleAPIStorageInterfaces(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()
	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr)
	defer serverInfo.Dispose()
	groupPath := "/apis/" + apiv2.GroupVersion.String()
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "interfaces"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	namespace.Status.Phase = apiv2.NamespacePhaseActive
	require.NoError(t, serverInfo.Client.Status().Update(ctx, namespace))
	namespace.Annotations = map[string]string{"test": "updated"}
	require.NoError(t, serverInfo.Client.Update(ctx, namespace))
	stored := &apiv2.Namespace{}
	require.NoError(t, serverInfo.Client.Get(ctx, namespace.NamespacedName(), stored))
	require.Equal(t, apiv2.NamespacePhaseActive, stored.Status.Phase)

	discoveryBody, discoveryErr := serverInfo.RestClient.Get().AbsPath(groupPath).DoRaw(ctx)
	require.NoError(t, discoveryErr)
	discovery := metav1.APIResourceList{}
	require.NoError(t, json.Unmarshal(discoveryBody, &discovery))
	foundNamespace, foundChild := false, false
	for _, resource := range discovery.APIResources {
		switch resource.Name {
		case "namespaces":
			foundNamespace = true
			require.False(t, resource.Namespaced)
			require.Contains(t, resource.ShortNames, "ns")
		case "physicalprocesses":
			foundChild = true
			require.True(t, resource.Namespaced)
		}
	}
	require.True(t, foundNamespace)
	require.True(t, foundChild)
	tableBody, tableErr := serverInfo.RestClient.Get().AbsPath(groupPath+"/namespaces").
		SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1").DoRaw(ctx)
	require.NoError(t, tableErr)
	table := metav1.Table{}
	require.NoError(t, json.Unmarshal(tableBody, &table))
	require.Len(t, table.Rows, 1)
}

func TestV2NamespaceLifecycleAPIDeleteDoesNotWaitForRequestBody(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultApiServerTestTimeout)
	defer cancel()
	serverInfo, startupErr := createApiServerForHttpHandlerTests(t.Name(), ctx)
	require.NoError(t, startupErr)
	defer serverInfo.Dispose()
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "partial-body"}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	serverURL, urlErr := url.Parse(serverInfo.ClientConfig.Host)
	require.NoError(t, urlErr)
	tlsConfig, tlsErr := clientgorest.TLSConfigFor(serverInfo.ClientConfig)
	require.NoError(t, tlsErr)
	tlsConfig.NextProtos = []string{"http/1.1"}
	dialer := tls.Dialer{Config: tlsConfig}
	connection, dialErr := dialer.DialContext(ctx, "tcp", serverURL.Host)
	require.NoError(t, dialErr)
	defer connection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetDeadline(deadline))

	pid := int64(2147483647)
	child := &apiv2.PhysicalProcess{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv2.GroupVersion.String(), Kind: "PhysicalProcess"},
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: namespace.Name},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
	body, marshalErr := json.Marshal(child)
	require.NoError(t, marshalErr)
	path := "/apis/" + apiv2.GroupVersion.String() + "/namespaces/" + namespace.Name + "/physicalprocesses"
	_, headerErr := fmt.Fprintf(connection,
		"POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n",
		path, serverURL.Host, serverInfo.ClientConfig.BearerToken, len(body),
	)
	require.NoError(t, headerErr)
	reader := bufio.NewReader(connection)
	request := &http.Request{Method: http.MethodPost}
	interim, interimErr := http.ReadResponse(reader, request)
	require.NoError(t, interimErr)
	require.Equal(t, http.StatusContinue, interim.StatusCode)
	require.NoError(t, interim.Body.Close())
	_, firstByteErr := connection.Write(body[:1])
	require.NoError(t, firstByteErr)

	require.NoError(t, serverInfo.Client.Delete(ctx, namespace))

	_, remainingErr := connection.Write(body[1:])
	require.NoError(t, remainingErr)
	response, responseErr := http.ReadResponse(reader, request)
	require.NoError(t, responseErr)
	defer response.Body.Close()
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}
