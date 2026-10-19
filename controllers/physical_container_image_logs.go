/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"errors"
	"fmt"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func (data *physicalContainerImageData) ensureStartupLogs(image *apiv2.PhysicalContainerImage) error {
	if data.startupStdout != nil {
		return nil
	}
	stdout, stdoutErr := logs.NewCaptureFile(image, logstream.LogStreamSourceStartupStdout)
	if stdoutErr != nil {
		return fmt.Errorf("create image startup stdout: %w", stdoutErr)
	}
	stderr, stderrErr := logs.NewCaptureFile(image, logstream.LogStreamSourceStartupStderr)
	if stderrErr != nil {
		return fmt.Errorf("create image startup stderr: %w", errors.Join(stderrErr, stdout.Close(logs.CaptureFileDisposeOnClose)))
	}
	data.startupStdout, data.startupStderr = stdout, stderr
	return nil
}

func (data *physicalContainerImageData) startupStreams() containers.StreamCommandOptions {
	data.startupStdout.Writer.NewParagraph()
	data.startupStderr.Writer.NewParagraph()
	return containers.StreamCommandOptions{
		StdOutStream: usvc_io.NopWriteCloser(data.startupStdout.Writer),
		StdErrStream: usvc_io.NopWriteCloser(data.startupStderr.Writer),
	}
}
