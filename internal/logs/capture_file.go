/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"errors"
	"os"
	"sync"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"

	"github.com/microsoft/dcp/internal/logstream"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// CaptureFile owns timestamped, numbered command output and supports paragraph separation.
// Its writer is not safe for concurrent use.
type CaptureFile struct {
	// Path is the capture file's filesystem path.
	Path string
	// Writer formats captured output and separates successive operations.
	Writer      usvc_io.ParagraphWriter
	closeOnce   func() error
	disposeOnce func() error
}

// NewCaptureFile opens a resource's output capture while preserving existing file contents.
func NewCaptureFile(resource apiserver_resource.Object, logSource logstream.LogStreamSource) (*CaptureFile, error) {
	metadata := resource.GetObjectMeta()
	fileName, nameErr := logstream.LogFileName(metadata.Name, metadata.UID, logSource, "")
	if nameErr != nil {
		return nil, nameErr
	}

	file, err := usvc_io.OpenOrCreateTempFileForAppending(fileName, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return nil, err
	}

	// Readers strip numbering and timestamps when the request does not ask for them.
	writer := usvc_io.NewParagraphWriter(usvc_io.NewTimestampWriter(file), osutil.LineSep())

	result := &CaptureFile{
		Path:   file.Name(),
		Writer: writer,
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

type CaptureFileCloseOption uint

const (
	CaptureFileCloseDefault   CaptureFileCloseOption = 0
	CaptureFileDisposeOnClose CaptureFileCloseOption = 1
)

// CloseAndLog closes the capture file, optionally deleting it, and logs any errors.
// Repeated calls are safe.
func (capture *CaptureFile) CloseAndLog(opt CaptureFileCloseOption, log logr.Logger) {
	closeErr := capture.Close(opt)
	if closeErr != nil {
		log.Error(closeErr, "Error closing capture log file", "File", capture.Path)
	}
}

// Close closes the capture file, optionally deleting it, and returns any errors.
// Repeated calls return the original close and deletion results.
func (capture *CaptureFile) Close(opt CaptureFileCloseOption) error {
	closeErr := capture.closeOnce()
	if opt&CaptureFileDisposeOnClose != 0 {
		return errors.Join(closeErr, capture.disposeOnce())
	} else {
		return closeErr
	}
}
