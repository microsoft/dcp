/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"io"
)

// ParagraphWriter is a WriteCloser that writes to an inner WriteCloser and has a concept of "paragraphs of data".
// A new paragraph is started by calling NewParagraph(). When that happens, the NEXT write
// (the first write of a new paragraph) will be preceded by a paragraph separator.
type ParagraphWriter interface {
	io.WriteCloser
	NewParagraph()
}

type paragraphWriter struct {
	inner        io.WriteCloser
	newParagraph bool
	sep          []byte
	firstWrite   bool
}

func NewParagraphWriter(w io.WriteCloser, paragraphSeparator []byte) ParagraphWriter {
	return &paragraphWriter{
		inner:        w,
		newParagraph: false,
		sep:          paragraphSeparator,
		firstWrite:   true,
	}
}

func (pw *paragraphWriter) Write(p []byte) (int, error) {
	if pw.newParagraph && !pw.firstWrite {
		pw.newParagraph = false
		if n, err := pw.inner.Write(pw.sep); err != nil || n != len(pw.sep) {
			return 0, err
		}
	}

	n, err := pw.inner.Write(p)
	if n > 0 {
		pw.firstWrite = false
	}
	return n, err
}

func (pw *paragraphWriter) Close() error {
	return pw.inner.Close()
}

func (pw *paragraphWriter) NewParagraph() {
	pw.newParagraph = true
}

var _ io.WriteCloser = (*paragraphWriter)(nil)
