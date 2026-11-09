/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
)

func enqueueOutputCleanup(queue *resiliency.WorkQueue, log logr.Logger, cleanup func(context.Context) error) {
	enqueueErr := queue.Enqueue(func(ctx context.Context) {
		if cleanupErr := cleanup(ctx); cleanupErr != nil {
			log.Error(cleanupErr, "Failed to clean up output files")
		}
	})
	if enqueueErr != nil {
		log.Error(enqueueErr, "Failed to queue output cleanup")
	}
}

func enqueueCaptureFileCleanup(queue *resiliency.WorkQueue, log logr.Logger, captures ...*logs.CaptureFile) {
	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		for _, capture := range captures {
			if capture != nil {
				capture.CloseAndLog(logs.CaptureFileCloseDefault, log)
			}
		}
		return
	}
	enqueueOutputCleanup(queue, log, func(ctx context.Context) error {
		for _, capture := range captures {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if capture != nil {
				capture.CloseAndLog(logs.CaptureFileDisposeOnClose, log)
			}
		}
		return nil
	})
}

func enqueueProcessOutputCleanup(queue *resiliency.WorkQueue, log logr.Logger, stdoutPath, stderrPath string) {
	if stdoutPath == "" && stderrPath == "" {
		return
	}
	enqueueOutputCleanup(queue, log, func(ctx context.Context) error {
		return logs.RemoveOutputFiles(ctx, stdoutPath, stderrPath)
	})
}
