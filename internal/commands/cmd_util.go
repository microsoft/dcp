/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"errors"
	"os"

	"github.com/microsoft/dcp/pkg/logger"
)

type exitErrorLogMode uint8

const (
	exitErrorLogAsError exitErrorLogMode = iota
	exitErrorLogAsInfo
	exitErrorLogSilent
)

type exitCodeError struct {
	err     error
	code    int
	logMode exitErrorLogMode
}

func NewExitCodeError(err error, exitCode int) error {
	return newExitCodeError(err, exitCode, exitErrorLogAsError)
}

// NewInformationalExitCodeError returns an exit-code error that ErrorExit logs at info level.
func NewInformationalExitCodeError(err error, exitCode int) error {
	return newExitCodeError(err, exitCode, exitErrorLogAsInfo)
}

// NewSilentExitCodeError returns an exit-code error that ErrorExit does not log.
// Use it when the command already emitted the appropriate scenario-specific diagnostic.
func NewSilentExitCodeError(err error, exitCode int) error {
	return newExitCodeError(err, exitCode, exitErrorLogSilent)
}

func newExitCodeError(err error, exitCode int, logMode exitErrorLogMode) error {
	return &exitCodeError{
		err:     err,
		code:    exitCode,
		logMode: logMode,
	}
}

func (e *exitCodeError) Error() string {
	return e.err.Error()
}

func (e *exitCodeError) Unwrap() error {
	return e.err
}

func (e *exitCodeError) ExitCode() int {
	return e.code
}

func ErrorExit(log *logger.Logger, err error, exitCode int) {
	exitCode, logMode := exitErrorMetadata(err, exitCode)
	switch logMode {
	case exitErrorLogSilent:
		// The command already emitted the scenario-specific diagnostic.
	case exitErrorLogAsInfo:
		log.Info("the program finished with an informational result", "Error", err, "ExitCode", exitCode)
	default:
		log.Error(err, "the program finished with an error", "ExitCode", exitCode)
	}

	log.Flush()
	os.Exit(exitCode)
}

func exitErrorMetadata(err error, fallbackExitCode int) (int, exitErrorLogMode) {
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), exitErr.logMode
	}
	return fallbackExitCode, exitErrorLogAsError
}
