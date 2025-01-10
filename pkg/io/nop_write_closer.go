package io

import (
	"io"
)

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

type nopWriteCloserReaderFrom struct {
	nopWriteCloser
	io.ReaderFrom
}

func NopWriteCloser(w io.Writer) io.WriteCloser {
	if r, ok := w.(io.ReaderFrom); ok {
		return nopWriteCloserReaderFrom{nopWriteCloser{w}, r}
	} else {
		return nopWriteCloser{w}
	}
}
