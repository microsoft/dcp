/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	apiv2 "github.com/microsoft/dcp/api/v2"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func removeContainerCopySources(ctx context.Context, config *apiv2.PhysicalContainerConfig, log logr.Logger) {
	if config == nil {
		return
	}
	for _, copyContext := range config.CopyFiles {
		if !copyContext.DeleteAfterCopy || copyContext.Source == "" {
			continue
		}
		removeCopyArchive(ctx, copyContext.Source, copyContext.SHA256, log)
	}
}

// removeCopyArchive removes a copied archive only while it still has its declared content, so a different file later
// written at the same path is not deleted.
func removeCopyArchive(ctx context.Context, source string, expectedSHA256 string, log logr.Logger) {
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
	archive, verifyErr := usvc_io.OpenSHA256VerifiedFile(ctx, source, expectedSHA256)
	if verifyErr != nil {
		log.V(1).Info("Retaining copy archive that no longer matches its declared content", "Source", source, "Reason", verifyErr.Error())
		return
	}
	if closeErr := archive.Close(); closeErr != nil {
		log.Error(closeErr, "Failed to close copy archive before removal", "Source", source)
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
