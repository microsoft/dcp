/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBridgeOutputHandler_StdoutCategory(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handler := newBridgeOutputHandler(stdout, stderr)

	handler.HandleOutput("stdout", "hello world\n")

	assert.Equal(t, "hello world\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestBridgeOutputHandler_StderrCategory(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handler := newBridgeOutputHandler(stdout, stderr)

	handler.HandleOutput("stderr", "error message\n")

	assert.Empty(t, stdout.String())
	assert.Equal(t, "error message\n", stderr.String())
}

func TestBridgeOutputHandler_ConsoleCategory(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handler := newBridgeOutputHandler(stdout, stderr)

	handler.HandleOutput("console", "console output\n")

	assert.Equal(t, "console output\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestBridgeOutputHandler_UnknownCategory(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handler := newBridgeOutputHandler(stdout, stderr)

	handler.HandleOutput("telemetry", "telemetry data\n")

	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestBridgeOutputHandler_NilWriters(t *testing.T) {
	t.Parallel()

	handler := newBridgeOutputHandler(nil, nil)

	// Should not panic with nil writers
	handler.HandleOutput("stdout", "hello\n")
	handler.HandleOutput("stderr", "error\n")
	handler.HandleOutput("console", "console\n")
}

func TestBridgeOutputHandler_NilStdoutOnly(t *testing.T) {
	t.Parallel()

	stderr := &bytes.Buffer{}
	handler := newBridgeOutputHandler(nil, stderr)

	handler.HandleOutput("stdout", "stdout\n")
	handler.HandleOutput("stderr", "stderr\n")

	assert.Equal(t, "stderr\n", stderr.String())
}

func TestBridgeOutputHandler_NilStderrOnly(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	handler := newBridgeOutputHandler(stdout, nil)

	handler.HandleOutput("stdout", "stdout\n")
	handler.HandleOutput("stderr", "stderr\n")

	assert.Equal(t, "stdout\n", stdout.String())
}
