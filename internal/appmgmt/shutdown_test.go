/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package appmgmt_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	clientgorest "k8s.io/client-go/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/appmgmt"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultShutdownTestTimeout = 60 * time.Second
	watchRegistrationDelay     = 2 * time.Second
)

// watchDelayingTransport delays every WATCH request before forwarding it to the API server.
type watchDelayingTransport struct {
	inner http.RoundTripper
}

func (wdt *watchDelayingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Query().Get("watch") == "true" {
		delayTimer := time.NewTimer(watchRegistrationDelay)
		defer delayTimer.Stop()
		select {
		case <-delayTimer.C:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return wdt.inner.RoundTrip(req)
}

// Verifies that cleanupResource completes successfully when objects are deleted before its watch is registered,
// i.e. that cleanup completion is determined by the objects that remain, not by observed deletion events.
func TestCleanupResourceCompletesWhenDeletionsPrecedeWatch(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultShutdownTestTimeout)
	defer cancel()

	sessionFolder, sessionFolderErr := testutil.CreateTestSessionDir()
	require.NoError(t, sessionFolderErr)
	log := testutil.NewLogWithResourceSinkForTesting(t.Name(), sessionFolder)

	serverInfo, startupErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone, log, sessionFolder)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer serverInfo.Dispose()

	const serviceCount = 5
	for i := range serviceCount {
		svc := &apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cleanup-probe-%d", i)},
			Spec: apiv1.ServiceSpec{
				Protocol:              commonapi.PortProtocolTCP,
				AddressAllocationMode: apiv1.AddressAllocationModeLocalhost,
			},
		}
		require.NoError(t, serverInfo.Client.Create(ctx, svc))
	}

	delayedWatchConfig := clientgorest.CopyConfig(serverInfo.ClientConfig)
	delayedWatchConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &watchDelayingTransport{inner: rt}
	})
	dynamicClient, dynamicClientErr := dynamic.NewForConfig(delayedWatchConfig)
	require.NoError(t, dynamicClientErr)

	cleanupErr := appmgmt.CleanupResource(ctx, serverInfo.Client, dynamicClient, (&apiv1.Service{}).GetGroupVersionResource(), log)
	require.NoError(t, cleanupErr)

	var remaining apiv1.ServiceList
	require.NoError(t, serverInfo.Client.List(ctx, &remaining))
	require.Empty(t, remaining.Items)
}
