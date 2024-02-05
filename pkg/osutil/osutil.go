package osutil

import (
	"runtime"
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
	if IsWindows() {
		b = append(b, crlf...)
	} else {
		b = append(b, lf...)
	}
	return b
}
