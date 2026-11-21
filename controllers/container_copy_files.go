/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	apiv2 "github.com/microsoft/dcp/api/v2"
)

func removeContainerCopySources(config *apiv2.PhysicalContainerConfig, log logr.Logger) {
	if config == nil {
		return
	}
	for _, copyContext := range config.CopyFiles {
		if !copyContext.DeleteAfterCopy || copyContext.Source == "" {
			continue
		}
		removeCopyArchive(copyContext.Source, log)
	}
}

func removeCopyArchive(source string, log logr.Logger) {
	info, inspectErr := os.Lstat(source)
	if errors.Is(inspectErr, os.ErrNotExist) {
		return
	}
	if inspectErr != nil {
		log.Error(inspectErr, "Failed to inspect copy archive for removal", "Source", source)
		return
	}
	if info.IsDir() {
		log.Error(fmt.Errorf("copy archive source is a directory"), "Refusing to remove directory as copy archive", "Source", source)
		return
	}
	if removeErr := os.Remove(source); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		log.Error(removeErr, "Failed to remove copy archive", "Source", source)
	}
}

func physicalContainerArchiveCleanupRequired(state physicalContainerState) bool {
	switch state {
	case physicalContainerStateDependencyFailed, physicalContainerStateCreateFailed, physicalContainerStateCleanupFailed,
		physicalContainerStateCreationSkipped, physicalContainerStateFileCopyFailed, physicalContainerStateStartFailed,
		physicalContainerStateExited, physicalContainerStateDead, physicalContainerStateMissing,
		physicalContainerStateStopped, physicalContainerStateInvalid:
		return true
	default:
		return false
	}
}
