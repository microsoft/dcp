/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

// BuildImageFromArchiveDirectory stages regular files and directories in a restricted
// workspace and builds from that directory. Archive ownership and Unix modes are not preserved.
func BuildImageFromArchiveDirectory(ctx context.Context, options BuildImageOptions, builder BuildImage) error {
	return buildImageFromArchiveDirectory(ctx, options, builder, usvc_io.DcpTempDir())
}

func buildImageFromArchiveDirectory(
	ctx context.Context,
	options BuildImageOptions,
	builder BuildImage,
	tempDirectory string,
) (returnErr error) {
	if options.ContainerBuildContext == nil || options.ContextArchive == nil {
		return fmt.Errorf("build context archive is required")
	}
	if options.Context != "" {
		return fmt.Errorf("build context path and build context archive are mutually exclusive")
	}
	if builder == nil {
		return fmt.Errorf("image builder is required")
	}
	if options.Timeout < 0 {
		return fmt.Errorf("image build timeout cannot be negative")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultBuildImageTimeout
	}
	buildCtx, buildCancel := context.WithTimeout(ctx, timeout)
	defer buildCancel()
	if contextErr := buildCtx.Err(); contextErr != nil {
		return contextErr
	}

	dockerfile := options.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfileRelativePath, dockerfilePathErr := archiveBuildContextPath(dockerfile)
	if dockerfilePathErr != nil {
		return fmt.Errorf("invalid archive Dockerfile path: %w", dockerfilePathErr)
	}

	workspace, workspaceErr := createBuildWorkspace(tempDirectory, "dcp-build-context-")
	if workspaceErr != nil {
		return workspaceErr
	}
	defer func() {
		returnErr = errors.Join(returnErr, wrapBuildWorkspaceCleanupError(workspace, os.RemoveAll(workspace)))
	}()

	if extractErr := extractBuildContextArchive(buildCtx, options.ContextArchive, workspace); extractErr != nil {
		return extractErr
	}

	dockerfilePath := filepath.Join(workspace, dockerfileRelativePath)
	dockerfileInfo, dockerfileStatErr := os.Lstat(dockerfilePath)
	if dockerfileStatErr != nil {
		return fmt.Errorf("inspect staged Dockerfile: %w", dockerfileStatErr)
	}
	if !dockerfileInfo.Mode().IsRegular() {
		return fmt.Errorf("staged Dockerfile is not a regular file")
	}
	if contextErr := buildCtx.Err(); contextErr != nil {
		return contextErr
	}

	stagedContext := *options.ContainerBuildContext
	stagedContext.Context = workspace
	stagedContext.ContextArchive = nil
	stagedContext.Dockerfile = dockerfilePath
	stagedOptions := options
	stagedOptions.ContainerBuildContext = &stagedContext
	if buildErr := builder.BuildImage(buildCtx, stagedOptions); buildErr != nil {
		return buildErr
	}
	return buildCtx.Err()
}

func extractBuildContextArchive(
	ctx context.Context,
	archive *ContainerBuildContextArchive,
	directory string,
) (returnErr error) {
	reader, openErr := OpenBuildContextArchive(ctx, archive)
	if openErr != nil {
		return openErr
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close build context archive: %w", closeErr))
		}
	}()

	archiveReader := tar.NewReader(reader)
	paths := make(map[string]struct{})
	pathSpellings := make(map[string]string)
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		header, nextErr := archiveReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return fmt.Errorf("read build context archive entry: %w", nextErr)
		}
		relativePath, pathErr := archiveBuildContextPath(header.Name)
		if pathErr != nil {
			return pathErr
		}
		if relativePath == "." && header.Typeflag != tar.TypeDir {
			return fmt.Errorf("build context archive entry cannot replace the workspace root")
		}
		pathKey := strings.ToLower(relativePath)
		if _, duplicate := paths[pathKey]; duplicate {
			return fmt.Errorf("duplicate build context archive path %q", header.Name)
		}
		paths[pathKey] = struct{}{}
		for componentPath := relativePath; componentPath != "."; componentPath = filepath.Dir(componentPath) {
			componentKey := strings.ToLower(componentPath)
			if originalPath, exists := pathSpellings[componentKey]; exists && originalPath != componentPath {
				return fmt.Errorf("case-colliding build context archive path %q", header.Name)
			}
			pathSpellings[componentKey] = componentPath
		}

		destination := filepath.Join(directory, relativePath)
		switch header.Typeflag {
		case tar.TypeDir:
			if directoryErr := usvc_io.EnsureRestrictedDirectory(destination, osutil.PermissionOnlyOwnerReadWriteTraverse); directoryErr != nil {
				return fmt.Errorf("stage build context directory %q: %w", header.Name, directoryErr)
			}
		case tar.TypeReg, tar.TypeRegA:
			if parentErr := usvc_io.EnsureRestrictedDirectory(filepath.Dir(destination), osutil.PermissionOnlyOwnerReadWriteTraverse); parentErr != nil {
				return fmt.Errorf("create build context parent directory for %q: %w", header.Name, parentErr)
			}
			if stageErr := writeBuildContextFile(ctx, destination, archiveReader, nil); stageErr != nil {
				return fmt.Errorf("stage build context file %q: %w", header.Name, stageErr)
			}
		default:
			return fmt.Errorf("unsupported build context archive entry type %d for %q", header.Typeflag, header.Name)
		}
	}
}

func archiveBuildContextPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00") || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe build context archive path %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." || (component != "." && strings.TrimRight(component, ". ") != component) {
			return "", fmt.Errorf("unsafe build context archive path %q", name)
		}
	}
	nativePath := filepath.FromSlash(path.Clean(name))
	if !filepath.IsLocal(nativePath) {
		return "", fmt.Errorf("unsafe build context archive path %q", name)
	}
	return nativePath, nil
}
