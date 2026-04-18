/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// A structure representing startup log file with an associated ParagraphWriter.
// Having a shared writer enables separation of logs from multiple startup activities
// (image build, container creation, container start, network configuration).
type startupLog struct {
	file        *os.File
	writer      usvc_io.ParagraphWriter
	closeOnce   func() error
	disposeOnce func() error
}

func newStartupLog(ctr *apiv1.Container, logSource apiv1.LogStreamSource) (*startupLog, error) {
	var fileNameTemplate string
	switch logSource {
	case apiv1.LogStreamSourceStartupStdout:
		fileNameTemplate = "%s_startout_%s"
	case apiv1.LogStreamSourceStartupStderr:
		fileNameTemplate = "%s_starterr_%s"
	default:
		return nil, fmt.Errorf("unknown log source %v", logSource) // Should never happen
	}

	file, err := usvc_io.OpenTempFile(fmt.Sprintf(fileNameTemplate, ctr.Name, ctr.UID), os.O_RDWR|os.O_CREATE|os.O_APPEND, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return nil, err
	}

	// Always append timestamp to startup logs; we'll strip them out if the streaming request doesn't ask for them
	writer := usvc_io.NewParagraphWriter(usvc_io.NewTimestampWriter(file), osutil.LineSep())

	result := &startupLog{
		file:   file,
		writer: writer,
		closeOnce: sync.OnceValue(func() error {
			closeErr := writer.Close()
			if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				return closeErr
			}
			return nil
		}),
		disposeOnce: sync.OnceValue(func() error {
			return os.Remove(file.Name())
		}),
	}
	return result, nil
}

type startupLogCloseOption uint

const (
	startupLogCloseOptionDefault startupLogCloseOption = 0
	startupLogDisposeOnClose     startupLogCloseOption = 1
)

// Close closes, and optionally disposes (deletes) the startup log file.
// Close is idempotent and safe to call multiple times. Any errors are logged using the provided logger.
func (sl *startupLog) Close(opt startupLogCloseOption, log logr.Logger) {
	closeErr := sl.doClose(opt)
	if closeErr != nil {
		log.Error(closeErr, "Error closing startup log file", "File", sl.file.Name())
	}
}

func (sl *startupLog) doClose(opt startupLogCloseOption) error {
	closeErr := sl.closeOnce()
	if opt&startupLogDisposeOnClose != 0 {
		return errors.Join(closeErr, sl.disposeOnce())
	} else {
		return closeErr
	}
}
