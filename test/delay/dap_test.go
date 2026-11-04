/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/dap"
)

// Verifies that the shared subprocess tool acknowledges disconnect and emits its final terminated event.
func TestDAPModeDisconnect(t *testing.T) {
	t.Parallel()
	var input, output bytes.Buffer
	require.NoError(t, dap.WriteMessage(&input, dap.Message{
		"seq": json.RawMessage("8"), "type": json.RawMessage(`"request"`), "command": json.RawMessage(`"disconnect"`),
	}))
	require.NoError(t, serveDAP(&input, &output))
	reader := bufio.NewReader(&output)
	response, responseErr := dap.ReadMessage(reader)
	require.NoError(t, responseErr)
	require.JSONEq(t, "8", string(response["request_seq"]))
	event, eventErr := dap.ReadMessage(reader)
	require.NoError(t, eventErr)
	require.JSONEq(t, `"terminated"`, string(event["event"]))
}
