// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"

	"github.com/microsoft/dcp/pkg/logger"
)

func NewLogForTesting(name string) logr.Logger {
	log := logger.New(name)
	log.SetLevel(zapcore.ErrorLevel)
	if !flag.Parsed() {
		flag.Parse() // Needed to test if verbose flag was present.
	}
	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}
	retval := log.Logger.WithValues("test", true)
	return retval
}
