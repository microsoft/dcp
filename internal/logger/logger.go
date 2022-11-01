package logger

import (
	"github.com/go-logr/logr"
	zapr "github.com/go-logr/zapr"
	kubezap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Create a logger, returning a flush function and error
func NewLogger(opts ...kubezap.Opts) (logr.Logger, func()) {
	zapLogger := kubezap.NewRaw(opts...)
	flushFn := func() {
		_ = zapLogger.Sync() // Best effort
	}
	logger := zapr.NewLogger(zapLogger)
	return logger, flushFn
}
