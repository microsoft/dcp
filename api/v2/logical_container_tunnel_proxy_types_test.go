/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Verifies that LogicalContainerTunnelProxy validates namespace-local references and unique tunnel names.
func TestLogicalContainerTunnelProxyValidation(t *testing.T) {
	valid := &LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "application"},
		Spec: LogicalContainerTunnelProxySpec{NetworkRef: "network", Tunnels: []LogicalContainerTunnel{
			{Name: "http", ServerServiceRef: "host", ClientServiceRef: "application/client"},
		}}}
	require.Empty(t, valid.Validate(t.Context()))
	cases := map[string]func(*LogicalContainerTunnelProxy){
		"missing network": func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.NetworkRef = "" },
		"foreign network": func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.NetworkRef = "other/network" },
		"foreign server":  func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.Tunnels[0].ServerServiceRef = "other/host" },
		"foreign client":  func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.Tunnels[0].ClientServiceRef = "other/client" },
		"duplicate name": func(proxy *LogicalContainerTunnelProxy) {
			proxy.Spec.Tunnels = append(proxy.Spec.Tunnels, proxy.Spec.Tunnels[0])
		},
		"invalid name":    func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.Tunnels[0].Name = "not valid" },
		"image injection": func(proxy *LogicalContainerTunnelProxy) { proxy.Spec.BaseImage = "base\nRUN dangerous" },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			proxy := valid.DeepCopy()
			modify(proxy)
			require.NotEmpty(t, proxy.Validate(t.Context()))
		})
	}
}

// Verifies that LogicalContainerTunnelProxy allows tunnel additions and removals but rejects redefinitions and immutable-field changes.
func TestLogicalContainerTunnelProxyUpdates(t *testing.T) {
	old := &LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "application"},
		Spec: LogicalContainerTunnelProxySpec{NetworkRef: "network", Aliases: []string{"a", "b"},
			Tunnels: []LogicalContainerTunnel{{Name: "http", ServerServiceRef: "host", ClientServiceRef: "client"}}}}
	for _, field := range []string{"network", "image", "aliases", "tunnel"} {
		t.Run(field, func(t *testing.T) {
			updated := old.DeepCopy()
			switch field {
			case "network":
				updated.Spec.NetworkRef = "different"
			case "image":
				updated.Spec.BaseImage = "different"
			case "aliases":
				updated.Spec.Aliases = nil
			case "tunnel":
				updated.Spec.Tunnels[0].ClientServiceRef = "different"
			}
			require.NotEmpty(t, updated.ValidateUpdate(t.Context(), old))
		})
	}
	added := old.DeepCopy()
	added.Spec.Tunnels = append(added.Spec.Tunnels, LogicalContainerTunnel{Name: "https", ServerServiceRef: "secure-host", ClientServiceRef: "secure-client"})
	require.Empty(t, added.ValidateUpdate(t.Context(), old))
	removed := old.DeepCopy()
	removed.Spec.Tunnels = nil
	require.Empty(t, removed.ValidateUpdate(t.Context(), old))
	reordered := old.DeepCopy()
	reordered.Spec.Aliases = []string{"b", "a"}
	require.Empty(t, reordered.ValidateUpdate(t.Context(), old))
}
