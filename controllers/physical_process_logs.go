/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
)

func (data *physicalProcessData) ensureOutputLogs(resource *apiv2.PhysicalProcess) error {
	if data.stdout != nil {
		return nil
	}
	var options []logs.CaptureFileOption
	if resource.Spec.Process.RetainRuntimeProcess {
		options = append(options, logs.CaptureFileDirectOutput)
	}
	stdout, stdoutErr := logs.NewCaptureFile(resource, logstream.LogStreamSourceStdout, options...)
	if stdoutErr != nil {
		return fmt.Errorf("create process stdout: %w", stdoutErr)
	}
	stderr, stderrErr := logs.NewCaptureFile(resource, logstream.LogStreamSourceStderr, options...)
	if stderrErr != nil {
		return fmt.Errorf("create process stderr: %w", errors.Join(stderrErr, stdout.Close(logs.CaptureFileDisposeOnClose)))
	}
	data.stdout, data.stderr = stdout, stderr
	return nil
}

func (data *physicalProcessData) closeOutputLogs(option logs.CaptureFileCloseOption, log logr.Logger) error {
	if data.stdout != nil {
		closeErr := errors.Join(data.stdout.Close(option), data.stderr.Close(option))
		if closeErr != nil {
			log.Error(closeErr, "Failed to close process output capture")
		}
		return closeErr
	}
	return nil
}
