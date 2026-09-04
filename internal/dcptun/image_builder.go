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

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/version"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/randdata"
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

type ClientProxyImagePlan struct {
	Image               string
	BuildContextArchive *containers.ContainerBuildContextArchive
	Dockerfile          string
	Labels              []containers.Label
}

func (p ClientProxyImagePlan) Cleanup() error {
	if p.BuildContextArchive == nil || p.BuildContextArchive.Source == "" {
		return nil
	}
	removeErr := os.Remove(p.BuildContextArchive.Source)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove client proxy image build context archive: %w", removeErr)
	}
	return nil
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
	plan, planErr := PrepareClientProxyImage(ctx, opts, ior, log)
	if planErr != nil {
		return "", planErr
	}
	defer func() { _ = plan.Cleanup() }()

	if plan.BuildContextArchive == nil {
		return plan.Image, nil
	}

	buildErr := ior.BuildImage(ctx, containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			ContextArchive: plan.BuildContextArchive,
			Dockerfile:     plan.Dockerfile,
			Tags:           []string{plan.Image},
			Labels:         plan.Labels,
		},
		TimeoutOption: containers.TimeoutOption{
			Timeout: opts.TimeoutOption.Timeout,
		},
		StreamCommandOptions: opts.StreamCommandOptions,
	})
	if buildErr != nil {
		return "", fmt.Errorf("failed to build client proxy image: %w", buildErr)
	}

	return plan.Image, nil
}

// PrepareClientProxyImage checks for a current client proxy image and prepares an archive build context when needed.
func PrepareClientProxyImage(
	ctx context.Context,
	opts BuildClientProxyImageOptions,
	ior containers.ImageOrchestrator,
	log logr.Logger,
) (ClientProxyImagePlan, error) {
	if ctx == nil {
		panic("context cannot be nil")
	}
	if ior == nil {
		panic("image orchestrator cannot be nil")
	}

	rtStat := ior.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
	if !rtStat.IsHealthy() {
		return ClientProxyImagePlan{}, &ErrContainerRuntimeUnhealthy{Reason: rtStat.Error}
	}

	if opts.BaseImage == "" {
		opts.BaseImage = DefaultBaseImage
	}

	dcpTunClientPath, clientPathErr := dcptunClientBinaryPath()
	if clientPathErr != nil {
		return ClientProxyImagePlan{}, fmt.Errorf("failed to get path to dcptun client binary: %w", clientPathErr)
	}

	imageName, imageErr := clientProxyImageName(dcpTunClientPath)
	if imageErr != nil {
		return ClientProxyImagePlan{}, fmt.Errorf("failed to determine client proxy image: %w", imageErr)
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
		return ClientProxyImagePlan{}, fmt.Errorf("failed to check if client proxy image needs to be built: %w", imageCheckErr)
	}
	if need == proxyImageExists {
		return ClientProxyImagePlan{Image: imageName}, nil
	}

	buildContextArchive, contextErr := setupImageBuildContextArchive(dcpTunClientPath, opts)
	if contextErr != nil {
		return ClientProxyImagePlan{}, fmt.Errorf("failed to create build context archive: %w", contextErr)
	}

	return ClientProxyImagePlan{
		Image:               imageName,
		BuildContextArchive: buildContextArchive,
		Dockerfile:          dockerfileName,
		Labels: []containers.Label{
			{
				Key:   baseImageDigestLabel,
				Value: string(baseImageDigests[opts.BaseImage]),
			},
		},
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

	haveImage, imageCheckErr := haveExistingImage(ctx, opts, ior, imageName, log)
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
	log logr.Logger,
) (bool, error) {
	baseImageDigest, found := baseImageDigests[opts.BaseImage]
	if !found {
		var digestErr error
		baseImageDigest, digestErr = getBestEffortBaseImageDigest(ctx, opts.BaseImage, ior, log)
		if digestErr != nil {
			return false, digestErr
		}

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

func getBestEffortBaseImageDigest(
	ctx context.Context,
	baseImage string,
	ior containers.ImageOrchestrator,
	log logr.Logger,
) (imageDigest, error) {
	baseImageID, pullErr := ior.PullImage(ctx, containers.PullImageOptions{Image: baseImage})
	if pullErr == nil {
		baseImageDigest, inspectErr := inspectBaseImageDigest(ctx, ior, baseImageID)
		if inspectErr != nil {
			return "", fmt.Errorf("failed to inspect client proxy base image %s: %w", baseImage, inspectErr)
		}

		return baseImageDigest, nil
	}

	baseImageDigest, inspectLocalErr := inspectBaseImageDigest(ctx, ior, baseImage)
	if inspectLocalErr != nil {
		return "", fmt.Errorf("failed to pull client proxy base image %s and no local copy was available: %w", baseImage, errors.Join(pullErr, inspectLocalErr))
	}

	log.V(1).Info("Failed to pull client proxy base image, using local image", "Image", baseImage, "Error", pullErr.Error())
	return baseImageDigest, nil
}

func inspectBaseImageDigest(
	ctx context.Context,
	ior containers.ImageOrchestrator,
	imageRef string,
) (imageDigest, error) {
	baseImageInspect, inspectErr := ior.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{imageRef}})
	if inspectErr != nil {
		return "", inspectErr
	}
	if len(baseImageInspect) == 0 {
		return "", fmt.Errorf("base image %s was not found", imageRef)
	}

	inspectedBaseImage := baseImageInspect[0]
	if inspectedBaseImage.Digest != "" {
		return imageDigest(inspectedBaseImage.Digest), nil
	}
	if inspectedBaseImage.Id != "" {
		return imageDigest(inspectedBaseImage.Id), nil
	}

	return "", fmt.Errorf("base image %s has no digest or ID", imageRef)
}

func setupImageBuildContextArchive(
	dcpTunClientPath string,
	opts BuildClientProxyImageOptions,
) (*containers.ContainerBuildContextArchive, error) {
	randomSuffix, randomSuffixErr := randdata.MakeRandomString(12)
	if randomSuffixErr != nil {
		return nil, fmt.Errorf("create random build context archive suffix: %w", randomSuffixErr)
	}
	archiveFile, openArchiveErr := usvc_io.OpenTempFile(
		fmt.Sprintf("dcptun-build-context-%s.tar", randomSuffix),
		os.O_RDWR|os.O_CREATE|os.O_EXCL,
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
`, opts.BaseImage, ClientBinaryName, ClientProxyBinaryPath)

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

	binaryFile, openBinaryErr := usvc_io.OpenFile(dcpTunClientPath, os.O_RDONLY, 0)
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
