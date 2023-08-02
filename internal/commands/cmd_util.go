package commands

import (
	"runtime"
)

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func WithNewline(b []byte) []byte {
	if IsWindows() {
		b = append(b, '\r')
	}
	b = append(b, '\n')
	return b
}
