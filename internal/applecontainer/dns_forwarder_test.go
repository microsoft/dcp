/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

func TestDnsRecordsRendering(t *testing.T) {
	t.Parallel()

	aco := &AppleContainerCliOrchestrator{log: logr.Discard()}

	// Aliases are recorded at creation time, deduplicated, with the container name first.
	aco.registerPendingDnsNames("myredis-abc123", []containers.CreateContainerNetworkOptions{
		{Name: "default", Aliases: []string{"myredis", "cache"}},
		{Name: "default", Aliases: []string{"myredis"}},
	})
	require.Equal(t, []string{"myredis-abc123", "myredis", "cache"}, aco.dns.pendingNames["myredis-abc123"])

	aco.dns.published = map[string]dnsRecord{
		"myredis-abc123": {ip: "192.168.64.7", names: aco.dns.pendingNames["myredis-abc123"]},
		"api-def456":     {ip: "192.168.64.9", names: []string{"api-def456", "api"}},
	}

	rendered := string(aco.renderRecordsLocked())
	require.Contains(t, rendered, "192.168.64.7 myredis-abc123 myredis cache\n")
	require.Contains(t, rendered, "192.168.64.9 api-def456 api\n")

	// Retracting a container's records removes only its entries.
	aco.unpublishDnsRecords([]string{"myredis-abc123", "never-published"})
	rendered = string(aco.renderRecordsLocked())
	require.NotContains(t, rendered, "myredis")
	require.Contains(t, rendered, "192.168.64.9 api-def456 api\n")
	require.NotContains(t, aco.dns.pendingNames, "myredis-abc123")
}
