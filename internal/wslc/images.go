/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/microsoft/dcp/internal/containers"
)

func (wco *WslcCliOrchestrator) BuildImage(ctx context.Context, options containers.BuildImageOptions) error {
	if options.ContainerBuildContext == nil {
		return fmt.Errorf("must specify a container build context")
	}
	if options.Context == "" {
		return fmt.Errorf("container build context path cannot be empty")
	}
	if options.Context == "-" {
		return fmt.Errorf("wslc requires a directory build context; stdin build contexts are unsupported")
	}
	if options.Platform != "" {
		return fmt.Errorf("wslc does not support selecting build platform %q", options.Platform)
	}

	args := []string{"image", "build"}
	if options.Dockerfile != "" {
		args = append(args, "--file", options.Dockerfile)
	}
	if options.Pull {
		args = append(args, "--pull")
	}
	if options.IidFile != "" {
		args = append(args, "--iidfile", options.IidFile)
	}
	for _, tag := range options.Tags {
		if tag == "" {
			return fmt.Errorf("image build tag cannot be empty")
		}
		args = append(args, "--tag", tag)
	}
	for _, buildArg := range options.Args {
		if buildArg.Name == "" {
			return fmt.Errorf("image build argument name cannot be empty")
		}
		if buildArg.Value == "" {
			args = append(args, "--build-arg", buildArg.Name)
		} else {
			args = append(args, "--build-arg", buildArg.Name+"="+buildArg.Value)
		}
	}

	secretEnvironment := make(map[string]string)
	for _, secret := range options.Secrets {
		if secret.ID == "" {
			return fmt.Errorf("image build secret ID cannot be empty")
		}

		switch secret.Type {
		case "", containers.FileSecret:
			if secret.Source == "" {
				return fmt.Errorf("file build secret %q must specify a source path", secret.ID)
			}
			args = append(args, "--secret", fmt.Sprintf("id=%s,type=file,src=%s", secret.ID, secret.Source))
		case containers.EnvSecret:
			environmentName := secret.Source
			if environmentName == "" {
				environmentName = secret.ID
			}
			args = append(args, "--secret", fmt.Sprintf("id=%s,type=env,env=%s", secret.ID, environmentName))
			if secret.Value != "" {
				secretEnvironment[environmentName] = secret.Value
			}
		default:
			return fmt.Errorf("unsupported image build secret type %q", secret.Type)
		}
	}

	if options.Stage != "" {
		args = append(args, "--target", options.Stage)
	}
	var labelArgsErr error
	args, labelArgsErr = appendLabelArgs(args, options.Labels, "image")
	if labelArgsErr != nil {
		return labelArgsErr
	}

	args = append(args, "--progress", "plain", options.Context)
	cmd := makeWslcCommand(args...)
	if len(secretEnvironment) > 0 {
		cmd.Env = os.Environ()
		secretNames := make([]string, 0, len(secretEnvironment))
		for secretName := range secretEnvironment {
			secretNames = append(secretNames, secretName)
		}
		sort.Strings(secretNames)
		for _, secretName := range secretNames {
			cmd.Env = append(cmd.Env, secretName+"="+secretEnvironment[secretName])
		}
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultBuildTimeout
	}
	_, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"BuildImage",
		cmd,
		options.StdOutStream,
		options.StdErrStream,
		timeout,
	)
	if runErr != nil {
		return errors.Join(runErr, normalizeCliErrors(errBuf, imageNotFoundMatch))
	}

	if options.IidFile != "" {
		if _, iidErr := containers.ReadImageIDFile(options.IidFile); iidErr != nil {
			return fmt.Errorf("validating WSLC image ID file %q: %w", options.IidFile, iidErr)
		}
	}
	return nil
}

func isImageIdentifier(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) > len("sha256:")
}

func (wco *WslcCliOrchestrator) InspectImages(ctx context.Context, options containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if len(options.Images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}

	args := append([]string{"image", "inspect", "--format", "json"}, options.Images...)
	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"InspectImages",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)

	rawImages, decodeErr := decodeJSONArray[wslcInspectedImage](outBuf)
	if runErr != nil {
		runErr = errors.Join(runErr, normalizeCliErrors(errBuf, imageNotFoundMatch.MaxObjects(len(options.Images))))
	}

	inspectedImages := make([]containers.InspectedImage, 0, len(rawImages))
	var conversionErr error
	for _, rawImage := range rawImages {
		imageID := strings.TrimSpace(rawImage.ID)
		if imageID == "" {
			conversionErr = errors.Join(
				conversionErr,
				containers.ErrUnmarshalling,
				fmt.Errorf("inspected WSLC image did not contain an ID"),
			)
			continue
		}
		inspectedImages = append(inspectedImages, containers.InspectedImage{
			Id:     imageID,
			Labels: rawImage.Config.Labels,
			Tags:   rawImage.RepoTags,
			Digest: imageDigest(rawImage.RepoDigests),
		})
	}

	return inspectedImages, errors.Join(
		runErr,
		decodeErr,
		conversionErr,
		incompleteError("images", len(inspectedImages), len(options.Images)),
	)
}

func (wco *WslcCliOrchestrator) PullImage(ctx context.Context, options containers.PullImageOptions) (string, error) {
	if options.Image == "" {
		return "", fmt.Errorf("must specify an image to pull")
	}

	imageReference := options.Image
	if options.Digest != "" {
		imageReference += "@" + options.Digest
	}
	cmd := makeWslcCommand("image", "pull", "--quiet", imageReference)

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultPullTimeout
	}
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"PullImage",
		cmd,
		nil,
		nil,
		timeout,
	)
	if runErr != nil {
		return "", errors.Join(runErr, normalizeCliErrors(errBuf, imageNotFoundMatch))
	}

	if imageID, idErr := parseSingleIdentifier(outBuf); idErr == nil && isImageIdentifier(imageID) {
		return imageID, nil
	}

	inspectedImages, inspectErr := wco.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageReference},
	})
	if inspectErr != nil {
		return "", fmt.Errorf("resolving pulled WSLC image ID: %w", inspectErr)
	}
	if len(inspectedImages) != 1 || inspectedImages[0].Id == "" {
		return "", fmt.Errorf("pulled WSLC image %q did not report an image ID", imageReference)
	}
	return inspectedImages[0].Id, nil
}

func (wco *WslcCliOrchestrator) RemoveImages(ctx context.Context, options containers.RemoveImagesOptions) ([]string, error) {
	return containers.RemoveImagesSequentially(ctx, options, func(removeCtx context.Context, image string, force bool) error {
		args := []string{"image", "remove"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, image)

		cmd := makeWslcCommand(args...)
		_, errBuf, runErr := wco.runBufferedWslcCommand(
			removeCtx,
			"RemoveImage",
			cmd,
			nil,
			nil,
			ordinaryCommandTimeout,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, imageNotFoundMatch, objectInUseMatch))
		}
		return nil
	})
}

func (wco *WslcCliOrchestrator) ApplyImageLayers(
	ctx context.Context,
	options containers.ApplyImageLayersOptions,
) (string, error) {
	return containers.ApplyImageLayersFromDirectory(ctx, wco.log, options, wco)
}
