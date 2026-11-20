/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package proto_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	stdproto "google.golang.org/protobuf/proto"

	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
)

// Verifies that TunnelSpec.LogString redacts DataConnectionToken, preserves non-secret
// tunnel metadata without mutating the receiver, and handles a nil receiver safely.
func TestTunnelSpecLogStringRedactsDataConnectionToken(t *testing.T) {
	t.Parallel()

	var nilSpec *dcptunproto.TunnelSpec
	require.Equal(t, "<nil>", nilSpec.LogString())

	secretToken := []byte("super-secret-tunnel-data-token-0123456789")
	spec := &dcptunproto.TunnelSpec{
		TunnelRef: &dcptunproto.TunnelRef{
			TunnelId: stdproto.Uint32(42),
		},
		ServerAddress:        stdproto.String("127.0.0.1"),
		ServerPort:           stdproto.Int32(18080),
		ClientProxyAddresses: []string{"10.89.0.5"},
		ClientProxyPort:      stdproto.Int32(28080),
		DataConnectionToken:  append([]byte(nil), secretToken...),
	}

	formatted := spec.LogString()
	require.Contains(t, formatted, "<redacted>")
	require.Contains(t, formatted, "42")
	require.Contains(t, formatted, "127.0.0.1")
	require.Contains(t, formatted, "18080")
	require.Contains(t, formatted, "10.89.0.5")
	require.Contains(t, formatted, "28080")

	require.NotContains(t, formatted, string(secretToken))
	require.NotContains(t, formatted, hex.EncodeToString(secretToken))
	require.NotContains(t, formatted, base64.StdEncoding.EncodeToString(secretToken))
	require.Equal(t, secretToken, spec.GetDataConnectionToken(), "LogString must not mutate the receiver")
}
