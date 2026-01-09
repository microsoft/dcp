/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	DCP_SESSION_FOLDER           = "DCP_SESSION_FOLDER"           // Folder to delete when finished with a session
	DCP_PRESERVE_EXECUTABLE_LOGS = "DCP_PRESERVE_EXECUTABLE_LOGS" // If truthy ("1", "true", "on", "yes"), preserve logs from executable runs
)

var (
	shouldCleanupSessionFolder bool = true
	DcpSessionDir              func() string
)

func PreserveSessionFolder() {
	shouldCleanupSessionFolder = false
}

func CleanupSessionFolderIfNeeded() {
	if !shouldCleanupSessionFolder {
		return
	}

	if osutil.EnvVarSwitchEnabled(DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if DcpSessionDir() != "" {
		if err := os.RemoveAll(DcpSessionDir()); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "failed to remove session directory: %v\n", err)
		}
	}
}

func init() {
	DcpSessionDir = sync.OnceValue(func() string {
		if dcpSessionDir, found := os.LookupEnv(DCP_SESSION_FOLDER); found {
			info, err := os.Stat(dcpSessionDir)
			if err == nil && info.IsDir() {
				return dcpSessionDir
			}
		}

		return ""
	})
}
