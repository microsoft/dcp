package osutil

import (
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
	b = append(b, LineSep()...)
	return b
}

func LineSep() []byte {
	if IsWindows() {
		return crlf
	} else {
		return lf
	}
}
