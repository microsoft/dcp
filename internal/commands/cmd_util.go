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

type exitCodeError struct {
	err      error
	exitCode int
}

func NewExitCodeError(err error, exitCode int) error {
	return &exitCodeError{
		err:      err,
		exitCode: exitCode,
	}
}

func (e *exitCodeError) Error() string {
	return e.err.Error()
}

func (e *exitCodeError) Unwrap() error {
	return e.err
}

func (e *exitCodeError) ExitCode() int {
	return e.exitCode
}

func ErrorExit(log *logger.Logger, err error, exitCode int) {
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}

	log.Error(err, "the program finished with an error", "ExitCode", exitCode)
	log.Flush()
	os.Exit(exitCode)
}
