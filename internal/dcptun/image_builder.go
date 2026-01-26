/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	// Default base image for client proxy containers
	DefaultBaseImage = "mcr.microsoft.com/azurelinux/base/core:3.0"

	// The interval at which we check whether the client proxy image has been built
	// (assuming another instance is building it).
	checkImageBuiltInterval = 5 * time.Second

	// How long are we willing to wait for the result on already-started image build.
	defaultImageBuildTimeout = 1 * time.Minute

	// The label containing the base image digest used for the client proxy image build.
	baseImageDigestLabel = "com.microsoft.developer.usvc-dev.base-image-digest"

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

var (
	// Protects critical sections of code that handles proxy image builds.
	imageBuildLock = concurrency.NewContextAwareLock()

	// Map (base image) --> (image digest) for client proxy image builds.
	// Used as means of verifying that the base image used for the build is the latest we can find,
	// and that the client proxy image is not stale.
	baseImageDigests = make(map[string]imageDigest)
)

type ErrContainerRuntimeUnhealthy struct {
	Reason string
}

type imageDigest string

func (e *ErrContainerRuntimeUnhealthy) Error() string {
	return fmt.Sprintf("container runtime is unhealthy: %s", e.Reason)
}

type BuildClientProxyImageOptions struct {
	BaseImage string
	containers.StreamCommandOptions
	containers.TimeoutOption

	// Overrides the most recent image builds file path.
	// Used primarily for testing purposes.
	MostRecentImageBuildsFilePath string
}

// EnsureClientProxyImage ensures that the client proxy image is built and available
// for use by the client proxy container.
// Returns full image name with tag, and error if any.
func EnsureClientProxyImage(
	ctx context.Context,
	opts BuildClientProxyImageOptions,
	ior containers.ImageOrchestrator,
	log logr.Logger,
) (string, error) {
	if ctx == nil {
		panic("context cannot be nil")
	}
	if ior == nil {
		panic("image orchestrator cannot be nil")
	}

	rtStat := ior.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	if !rtStat.IsHealthy() {
		return "", &ErrContainerRuntimeUnhealthy{Reason: rtStat.Error}
	}

	if opts.BaseImage == "" {
		opts.BaseImage = DefaultBaseImage
	}

	dcpTunClientPath, clientPathErr := dcptunClientBinaryPath()
	if clientPathErr != nil {
		return "", fmt.Errorf("failed to get path to dcptun client binary: %w", clientPathErr)
	}

	imageName, imageErr := clientProxyImageName(dcpTunClientPath)
	if imageErr != nil {
		return "", fmt.Errorf("failed to determine client proxy image: %w", imageErr)
	}

	errKeepWaiting := errors.New("waiting for client proxy image to be built...")

	need, imageCheckErr := resiliency.RetryGet(ctx, backoff.NewConstantBackOff(checkImageBuiltInterval), func() (clientProxyImageNeed, error) {
		res, err := shouldBuildClientProxyImage(ctx, opts, ior, imageName, log)
		if err != nil {
			return proxyImageNeedUnknown, backoff.Permanent(err)
		}
		if res == proxyImageWait {
			return proxyImageWait, errKeepWaiting
		}
		return res, nil
	})
	if imageCheckErr != nil {
		return "", fmt.Errorf("failed to check if client proxy image needs to be built: %w", imageCheckErr)
	}
	if need == proxyImageExists {
		return imageName, nil
	}

	// Create build context with Dockerfile
	buildContext, cleanup, contextErr := setupImageBuildContext(dcpTunClientPath, opts)
	if contextErr != nil {
		return "", fmt.Errorf("failed to create build context: %w", contextErr)
	}
	defer cleanup()

	// Build the image
	buildOptions := containers.BuildImageOptions{
		ContainerBuildContext: &apiv1.ContainerBuildContext{
			Context:    buildContext,
			Dockerfile: filepath.Join(buildContext, dockerfileName),
			Tags:       []string{imageName},
			Labels: []apiv1.ContainerLabel{
				{
					Key:   baseImageDigestLabel,
					Value: string(baseImageDigests[opts.BaseImage]),
				},
			},
		},
		TimeoutOption: containers.TimeoutOption{
			Timeout: opts.TimeoutOption.Timeout,
		},
	}
	if opts.StdOutStream != nil {
		buildOptions.StdOutStream = opts.StdOutStream
	}
	if opts.StdErrStream != nil {
		buildOptions.StdErrStream = opts.StdErrStream
	}

	buildErr := ior.BuildImage(ctx, buildOptions)
	if buildErr != nil {
		return "", fmt.Errorf("failed to build client proxy image: %w", buildErr)
	}

	return imageName, nil
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

type clientProxyImageNeed string

const (
	proxyImageNeedUnknown clientProxyImageNeed = "unknown" // An error occurred and the image state is unknown
	proxyImageExists      clientProxyImageNeed = "exists"  // The image already exists and is up to date
	proxyImageBuild       clientProxyImageNeed = "build"   // The image needs to be built
	proxyImageWait        clientProxyImageNeed = "wait"    // The image is being built, wait for it to finish
)

// Figures out whether existing  client proxy image is up to date, or a new one is being built and we need to wait for it,
// or if we are the party that needs to build the image. Returns the "need" for the image, and an error if any.
// If no error occurs, the baseImageDigests map is guaranteed to contain the base image digest for the given base image.
func shouldBuildClientProxyImage(
	ctx context.Context,
	opts BuildClientProxyImageOptions,
	ior containers.ImageOrchestrator,
	imageName string,
	log logr.Logger,
) (clientProxyImageNeed, error) {
	lockErr := imageBuildLock.Lock(ctx)
	if lockErr != nil {
		return proxyImageNeedUnknown, lockErr // Context expired.
	}
	defer imageBuildLock.Unlock()

	haveImage, imageCheckErr := haveExistingImage(ctx, opts, ior, imageName)
	if imageCheckErr != nil {
		return proxyImageNeedUnknown, fmt.Errorf("failed to check if the client proxy image exists: %w", imageCheckErr)
	}
	if haveImage {
		// The image exists and is up to date
		return proxyImageExists, nil // Image exists and is up to date
	}

	var imageBuildsFile *imageBuildsFile
	var fileErr error
	if opts.MostRecentImageBuildsFilePath != "" {
		imageBuildsFile, fileErr = newImageBuildsFile(opts.MostRecentImageBuildsFilePath)
		defer func() { _ = imageBuildsFile.Close() }() // best effort
	} else {
		imageBuildsFile, fileErr = getDefaultImageBuildsFile()
	}
	if fileErr != nil {
		return proxyImageNeedUnknown, fmt.Errorf("failed to get the most recent image builds file: %w", fileErr)
	}

	imageBuilds, imageBuildsErr := imageBuildsFile.tryLockAndRead(ctx, defaultImageBuildTimeout)
	if imageBuildsErr != nil {
		return proxyImageNeedUnknown, fmt.Errorf("failed to check whether other DCP instances are already building the image: %w", imageBuildsErr)
	}
	defer func() {
		// Note: Unlock() is no-op if the file is already unlocked
		if unlockErr := imageBuildsFile.Unlock(); unlockErr != nil {
			log.Error(unlockErr, "Failed to unlock the most recent image builds file after checking if the image is being built") // Should never happen
		}
	}()

	baseImageDigest := baseImageDigests[opts.BaseImage] // The map has it because we called haveExistingImage() above
	alreadyBuilding := slices.Any(imageBuilds, func(r imageBuildRecord) bool {
		return r.ImageName == imageName && r.BaseImageDigest == baseImageDigest
	})
	if alreadyBuilding {
		log.V(1).Info("The client proxy image is already being built, waiting for it to finish", "ImageName", imageName)
		return proxyImageWait, nil
	}

	// We need to build the image
	imageBuilds = append(imageBuilds, imageBuildRecord{
		ImageName:       imageName,
		BaseImageDigest: baseImageDigests[opts.BaseImage],
		Instance:        networking.GetProgramInstanceID(),
		Timestamp:       time.Now(),
	})
	if writeErr := imageBuildsFile.WriteAndUnlock(ctx, imageBuilds); writeErr != nil {
		log.Error(writeErr, "Failed to write the most recent image builds file while checking if the image is being built")
	}
	return proxyImageBuild, nil
}

// Tries to check for existing image and verify its freshness.
// Assumes imageBuildLock is already held.
// Returns true if the image exists and is up to date, false if it does not exist, and an error if any.
// If no error occurs, the baseImageDigests map is guaranteed to contain the base image digest for the given base image.
func haveExistingImage(
	ctx context.Context,
	opts BuildClientProxyImageOptions,
	ior containers.ImageOrchestrator,
	imageName string,
) (bool, error) {
	baseImageDigest, found := baseImageDigests[opts.BaseImage]
	if !found {
		baseImageID, pullErr := ior.PullImage(ctx, containers.PullImageOptions{Image: opts.BaseImage})
		if pullErr != nil {
			return false, fmt.Errorf("failed to pull client proxy base image %s: %w", opts.BaseImage, pullErr)
		}

		baseImageInspect, inspectErr := ior.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{baseImageID}})
		if inspectErr != nil {
			return false, fmt.Errorf("failed to inspect client proxy base image %s: %w", opts.BaseImage, inspectErr)
		}

		baseImageDigest = imageDigest(baseImageInspect[0].Digest)
		baseImageDigests[opts.BaseImage] = baseImageDigest
	}

	images, err := ior.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageName},
	})
	if err != nil && !errors.Is(err, containers.ErrNotFound) {
		return false, fmt.Errorf("failed to inspect client proxy image %s: %w", imageName, err)
	}

	if len(images) == 0 {
		return false, nil // Image does not exist
	}

	existing := images[0]
	retval := imageDigest(existing.Labels[baseImageDigestLabel]) == baseImageDigest
	return retval, nil
}

// setupImageBuildContext creates a temporary directory for building the client proxy image,
// creates a Dockerfile, and makes the client binary appear in the build context.
// Returns the path to the build context, a cleanup function to remove it, and an error if any.
func setupImageBuildContext(dcpTunClientPath string, opts BuildClientProxyImageOptions) (string, func(), error) {
	// Create temporary directory for build context
	tempDir, tempDirErr := os.MkdirTemp(usvc_io.DcpTempDir(), "dcptun-build-*")
	if tempDirErr != nil {
		return "", nil, fmt.Errorf("failed to create temporary directory: %w", tempDirErr)
	}

	cleanup := func() {
		_ = os.RemoveAll(tempDir) // best effort
	}

	// Make the client proxy binary appear the build context
	// One would think that creating a symlink would be the right thing to do here,
	// but Docker build does not follow symlinks outside the build context.
	// So we have to copy the binary instead.
	// If this turns to be a performance issue, we can look into using Docker buildkit
	// with "additional", external context pointing to the bin folder.
	destBinaryPath := filepath.Join(tempDir, ClientBinaryName)
	if copyErr := copyFile(dcpTunClientPath, destBinaryPath, osutil.PermissionOnlyOwnerReadWriteExecute); copyErr != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to copy binary to build context: %w", copyErr)
	}

	dockerfileContent := fmt.Sprintf(`
FROM %s

# Copy the dcptun client binary
COPY --chmod=0755 %s %[3]s

# Set the entrypoint to the dcptun client
ENTRYPOINT ["%[3]s"]
`, opts.BaseImage, ClientBinaryName, ClientProxyBinaryPath)

	// Write Dockerfile
	dockerfilePath := filepath.Join(tempDir, dockerfileName)
	if err := usvc_io.WriteFile(dockerfilePath, []byte(dockerfileContent), osutil.PermissionOnlyOwnerReadWrite); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	return tempDir, cleanup, nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string, perm os.FileMode) error {
	sourceFile, sourceErr := usvc_io.OpenFile(src, os.O_RDONLY, 0)
	if sourceErr != nil {
		return sourceErr
	}
	defer func() { _ = sourceFile.Close() }()

	destFile, destErr := usvc_io.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if destErr != nil {
		return destErr
	}

	if _, copyErr := io.Copy(destFile, sourceFile); copyErr != nil {
		_ = destFile.Close()
		return copyErr
	}

	return destFile.Close()
}

// Computes the SHA256 hash of a given binary file
func computeFileHash(filePath string) (string, error) {
	file, openErr := os.Open(filePath)
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
	dcpBinaryPath, pathErr := filepath.Abs(os.Args[0])
	if pathErr != nil {
		return "", fmt.Errorf("failed to get absolute path to dcp binary: %w", pathErr)
	}

	binDir := filepath.Dir(dcpBinaryPath)
	binaryPath := filepath.Join(binDir, ClientBinaryName)
	fi, statErr := os.Stat(binaryPath)

	// Verify the binary exists
	if statErr == nil && fi.Mode().IsRegular() {
		return binaryPath, nil
	}

	// Fallback to the DCP bin directory (used primarily for testing)
	binDir, binDirErr := dcppaths.GetDcpBinDir()
	if binDirErr != nil {
		return "", fmt.Errorf("dcptun client binary not found next to the running binary and DCP bin directory location could not be determined: %w", binDirErr)
	}

	binaryPath = filepath.Join(binDir, ClientBinaryName)
	fi, statErr = os.Stat(binaryPath)

	if statErr != nil {
		return "", fmt.Errorf("dcptun client binary not found at %s: %w", binaryPath, statErr)
	}

	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("dcptun client binary at %s is not a regular file", binaryPath)
	}

	return binaryPath, nil
}
