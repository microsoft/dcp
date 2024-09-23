package testutil

import (
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewLogForTesting(name string) logr.Logger {
	log := logger.New(name)
	log.SetLevel(zapcore.ErrorLevel)
	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}
	retval := log.Logger.WithValues("test", true)
	return retval
}
