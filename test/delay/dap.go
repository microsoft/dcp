/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
		var arguments struct {
			Response string `json:"_testResponse"`
		}
		if raw, present := message["arguments"]; present {
			if argumentsErr := json.Unmarshal(raw, &arguments); argumentsErr != nil {
				return argumentsErr
			}
		}
		switch arguments.Response {
		case "defer":
			continue
		case "", "reject":
		default:
			return fmt.Errorf("unsupported test response behavior %q", arguments.Response)
		}
		if command == "reportProcess" {
			sequence++
			if writeErr := dap.WriteMessage(output, dap.Message{
				"seq": json.RawMessage(strconv.FormatInt(sequence, 10)), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"process"`), "body": message["arguments"],
			}); writeErr != nil {
				return writeErr
			}
		}
		sequence++
		response := dap.Message{
			"seq": json.RawMessage(strconv.FormatInt(sequence, 10)), "type": json.RawMessage(`"response"`),
			"request_seq": message["seq"], "command": message["command"], "success": json.RawMessage("true"),
		}
		if arguments.Response == "reject" {
			response["success"] = json.RawMessage("false")
			response["message"] = json.RawMessage(`"test adapter rejected request"`)
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
