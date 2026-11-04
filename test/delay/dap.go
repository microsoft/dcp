/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/microsoft/dcp/internal/dap"
)

func serveDAP(input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	sequence := int64(0)
	for {
		message, readErr := dap.ReadMessage(reader)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		var kind, command string
		if typeErr := json.Unmarshal(message["type"], &kind); typeErr != nil {
			return typeErr
		}
		if kind != "request" {
			continue
		}
		if commandErr := json.Unmarshal(message["command"], &command); commandErr != nil {
			return commandErr
		}
		sequence++
		response := dap.Message{
			"seq": json.RawMessage(strconv.FormatInt(sequence, 10)), "type": json.RawMessage(`"response"`),
			"request_seq": message["seq"], "command": message["command"], "success": json.RawMessage("true"),
		}
		if writeErr := dap.WriteMessage(output, response); writeErr != nil {
			return writeErr
		}
		if command == "disconnect" {
			sequence++
			return dap.WriteMessage(output, dap.Message{
				"seq": json.RawMessage(strconv.FormatInt(sequence, 10)), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"terminated"`),
			})
		}
	}
}
