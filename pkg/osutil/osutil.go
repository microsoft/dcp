package osutil

import (
	"bytes"
	"runtime"
)

const (
	MaxCopyFileSize = 50 * 1024 * 1024 // 50MB
)

var (
	lf   = []byte("\n")
	crlf = []byte("\r\n")
)

func LF() []byte {
	return lf
}

func CRLF() []byte {
	return crlf
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func WithNewline(b []byte) []byte {
	// Do not modify the original slice (e.g. don't do ret = append(b, '\n'))
	var retval []byte
	if IsWindows() {
		retval = bytes.Join([][]byte{b, crlf}, nil)
	} else {
		retval = bytes.Join([][]byte{b, lf}, nil)
	}
	return retval
}

func LineSep() []byte {
	if IsWindows() {
		return crlf
	} else {
		return lf
	}
}
