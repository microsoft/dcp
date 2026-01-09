/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"os"

	"github.com/microsoft/dcp/pkg/logger"
)

func ErrorExit(log *logger.Logger, err error, exitCode int) {
	log.Error(err, "the program finished with an error", "ExitCode", exitCode)
	log.Flush()
	os.Exit(exitCode)
}
