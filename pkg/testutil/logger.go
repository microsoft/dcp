/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"

	"github.com/microsoft/dcp/pkg/logger"
)

func NewLogForTesting(name string) logr.Logger {
	log := makeTestLogger(name)
	return log.Logger
}

func NewLogWithResourceSinkForTesting(name string, resourceLogFolder string) logr.Logger {
	log := makeTestLogger(name).WithResourceSinkInto(resourceLogFolder)
	return log.Logger
}

func makeTestLogger(name string) *logger.Logger {
	log := logger.New(name)
	log.SetLevel(zapcore.ErrorLevel)
	if !flag.Parsed() {
		flag.Parse() // Needed to test if verbose flag was present.
	}
	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}
	log.Logger = log.Logger.WithValues("test", true)
	return log
}
