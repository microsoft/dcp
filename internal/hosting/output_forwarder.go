/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hosting

import (
	"bufio"
	"errors"
	"io"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/osutil"
)

// maxForwardedLineSize is the longest line that forwardLines writes as a single unit.
const maxForwardedLineSize = 1024 * 1024

// forwardLines copies src to dst one line at a time until src returns an error (normally io.EOF).
// Complete input line endings are preserved. Lines longer than maxForwardedLineSize are split into pieces
// terminated with the platform-appropriate line separator, and a warning is logged for every split.
// Any final unterminated input is also terminated with the platform-appropriate line separator.
// Write errors are ignored and src is drained until it returns an error, so that the producer of src is
// never blocked by, or exposed to, a broken destination.
func forwardLines(src io.Reader, dst io.Writer, streamName string, log logr.Logger) {
	reader := bufio.NewReaderSize(src, maxForwardedLineSize)
	var piece []byte

	for {
		line, readErr := reader.ReadSlice('\n')

		if readErr == nil {
			_, _ = dst.Write(line)
			continue
		}

		if len(line) > 0 {
			if errors.Is(readErr, bufio.ErrBufferFull) {
				log.Info("Warning: output line exceeds the maximum line size and will be split",
					"Stream", streamName,
					"MaxLineSize", maxForwardedLineSize,
				)
			}

			// Writing the piece and its terminator in one call keeps them together when dst is shared.
			piece = append(append(piece[:0], line...), osutil.LineSep()...)
			_, _ = dst.Write(piece)
		}

		if !errors.Is(readErr, bufio.ErrBufferFull) {
			return
		}
	}
}
