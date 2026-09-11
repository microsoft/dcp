/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/version"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/randdata"
)

const (
	// Default base image for client proxy containers
	DefaultBaseImage = "mcr.microsoft.com/azurelinux/base/core:3.0"

	dockerfileName = "Dockerfile"
)

const (
	// Default port for the control endpoint of the client-side tunnel proxy (container network side).
	DefaultContainerProxyControlPort = 15049

	// Default port for the data endpoint of the client-side tunnel proxy (container network side).
	DefaultContainerProxyDataPort = 15050

	// Full path to the client proxy binary inside the container image.
	ClientProxyBinaryPath = "/usr/local/bin/" + ClientBinaryName

	ClientProxyContainerImageNamePrefix = "dcptun_developer_ms"
)

type ClientProxyImageBuildPlan struct {
	Image               string
	BuildContextArchive *containers.ContainerBuildContextArchive
	Dockerfile          string
}

// PrepareClientProxyImageBuild creates the build input for the shared tunnel proxy
// PhysicalContainerImage.
func PrepareClientProxyImageBuild() (ClientProxyImageBuildPlan, error) {
	dcpTunClientPath, clientPathErr := dcptunClientBinaryPath()
	if clientPathErr != nil {
		return ClientProxyImageBuildPlan{}, fmt.Errorf("failed to get path to dcptun client binary: %w", clientPathErr)
	}

	imageName, imageErr := clientProxyImageName(dcpTunClientPath)
	if imageErr != nil {
		return ClientProxyImageBuildPlan{}, fmt.Errorf("failed to determine client proxy image: %w", imageErr)
	}

	buildContextArchive, contextErr := setupImageBuildContextArchive(dcpTunClientPath)
	if contextErr != nil {
		return ClientProxyImageBuildPlan{}, fmt.Errorf("failed to create build context archive: %w", contextErr)
	}

	return ClientProxyImageBuildPlan{
		Image:               imageName,
		BuildContextArchive: buildContextArchive,
		Dockerfile:          dockerfileName,
	}, nil
}

// clientProxyImageName() determines the name of the client proxy container image,
// based on the current version of the DCP binaries.
func clientProxyImageName(dcpTunClientPath string) (string, error) {
	imageName := ClientProxyContainerImageNamePrefix

	tag := version.Version().Version

	if tag == version.DevelopmentVersion {
		// Compute the hash of our binary and append it to the tag
		hash, hashErr := computeFileHash(dcpTunClientPath)
		if hashErr != nil {
			return "", fmt.Errorf("failed to compute current executable hash: %w", hashErr)
		}

		// 12 characters is more than enough for ensuring that the image with correct binary exists
		tag += "_" + hash[:12]
	}

	return fmt.Sprintf("%s:%s", imageName, tag), nil
}

func setupImageBuildContextArchive(
	dcpTunClientPath string,
) (*containers.ContainerBuildContextArchive, error) {
	randomSuffix, randomSuffixErr := randdata.MakeRandomString(12)
	if randomSuffixErr != nil {
		return nil, fmt.Errorf("create random build context archive suffix: %w", randomSuffixErr)
	}
	// The archive lives in the DCP session directory, whose lifetime owns its cleanup.
	archiveFile, openArchiveErr := usvc_io.CreateNewTempFile(
		fmt.Sprintf("dcptun-build-context-%s.tar", randomSuffix),
		osutil.PermissionOnlyOwnerReadWrite,
	)
	if openArchiveErr != nil {
		return nil, fmt.Errorf("create build context archive: %w", openArchiveErr)
	}
	archivePath := archiveFile.Name()
	cleanup := func() {
		_ = archiveFile.Close()
		_ = os.Remove(archivePath)
	}

	dockerfileContent := fmt.Sprintf(`
FROM %s

# Copy the dcptun client binary
COPY --chmod=0755 %s %[3]s

# Set the entrypoint to the dcptun client
ENTRYPOINT ["%[3]s"]
`, DefaultBaseImage, ClientBinaryName, ClientProxyBinaryPath)

	now := time.Now()
	tarWriter := usvc_io.NewTarWriterTo(archiveFile)
	if writeDockerfileErr := tarWriter.WriteFile(
		[]byte(dockerfileContent),
		dockerfileName,
		0,
		0,
		osutil.PermissionOwnerReadWriteOthersRead,
		now,
		now,
		now,
	); writeDockerfileErr != nil {
		cleanup()
		return nil, fmt.Errorf("write Dockerfile to build context archive: %w", writeDockerfileErr)
	}

	binaryFile, openBinaryErr := usvc_io.OpenFileReadOnly(dcpTunClientPath)
	if openBinaryErr != nil {
		cleanup()
		return nil, fmt.Errorf("open dcptun client binary: %w", openBinaryErr)
	}
	binaryInfo, statBinaryErr := binaryFile.Stat()
	if statBinaryErr != nil {
		_ = binaryFile.Close()
		cleanup()
		return nil, fmt.Errorf("stat dcptun client binary: %w", statBinaryErr)
	}
	copyBinaryErr := tarWriter.CopyFile(
		binaryFile,
		binaryInfo.Size(),
		ClientBinaryName,
		0,
		0,
		os.FileMode(0o755),
		binaryInfo.ModTime(),
		binaryInfo.ModTime(),
		binaryInfo.ModTime(),
	)
	closeBinaryErr := binaryFile.Close()
	if copyBinaryErr != nil {
		cleanup()
		return nil, fmt.Errorf("copy dcptun client binary to build context archive: %w", copyBinaryErr)
	}
	if closeBinaryErr != nil {
		cleanup()
		return nil, fmt.Errorf("close dcptun client binary: %w", closeBinaryErr)
	}
	if closeTarErr := tarWriter.Close(); closeTarErr != nil {
		cleanup()
		return nil, fmt.Errorf("finalize build context archive: %w", closeTarErr)
	}
	if closeArchiveErr := archiveFile.Close(); closeArchiveErr != nil {
		cleanup()
		return nil, fmt.Errorf("close build context archive: %w", closeArchiveErr)
	}

	archiveHash, hashErr := computeFileHash(archivePath)
	if hashErr != nil {
		cleanup()
		return nil, fmt.Errorf("hash build context archive: %w", hashErr)
	}

	return &containers.ContainerBuildContextArchive{
		Digest: "sha256:" + archiveHash,
		Source: archivePath,
		SHA256: archiveHash,
	}, nil
}

// Computes the SHA256 hash of a given binary file
func computeFileHash(filePath string) (string, error) {
	file, openErr := usvc_io.OpenFileReadOnly(filePath)
	if openErr != nil {
		return "", fmt.Errorf("failed to open binary file %s: %w", filePath, openErr)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, file); copyErr != nil {
		return "", fmt.Errorf("failed to compute hash of binary %s: %w", filePath, copyErr)
	}

	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

// Returns the path to the dcptun_c binary
func dcptunClientBinaryPath() (string, error) {
	dcpDir, dcpDirErr := dcppaths.GetDcpDir()
	if dcpDirErr != nil {
		return "", fmt.Errorf("failed to get DCP directory: %w", dcpDirErr)
	}

	binaryPath := filepath.Join(dcpDir, ClientBinaryName)
	fi, statErr := os.Stat(binaryPath)

	// Verify the binary exists
	if statErr == nil && fi.Mode().IsRegular() {
		return binaryPath, nil
	}

	// Fallback: probe for dcptun_c from the current directory (used primarily for testing)
	rootFolder, rootFindErr := osutil.FindRootFor(osutil.FileTarget, dcppaths.BuildOutputDir, ClientBinaryName)
	if rootFindErr != nil {
		return "", fmt.Errorf("dcptun client binary not found next to the running binary and could not be located via filesystem probing: %w", rootFindErr)
	}

	binaryPath = filepath.Join(rootFolder, dcppaths.BuildOutputDir, ClientBinaryName)
	fi, statErr = os.Stat(binaryPath)

	if statErr != nil {
		return "", fmt.Errorf("dcptun client binary not found at %s: %w", binaryPath, statErr)
	}

	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("dcptun client binary at %s is not a regular file", binaryPath)
	}

	return binaryPath, nil
}
