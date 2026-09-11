/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"context"
	"errors"
	"fmt"
)

// InspectImages command types

type InspectedImage struct {
	// ID of the image
	Id string `json:"Id"`

	// Labels applied to the image
	Labels map[string]string `json:"Labels,omitempty"`

	// Tags applied to the image
	Tags []string `json:"Tags,omitempty"`

	// Digest of the image
	Digest string `json:"Digest,omitempty"`
}

type InspectImagesOptions struct {
	// The list of image IDs or names to inspect
	Images []string
}

type InspectImages interface {
	// Inspect images returns a list of InspectedImage objects for the given image IDs or names.
	// This method may partially succeed, returning a subset of images that were successfully inspected,
	// and a list of errors from stderr responses or errors in unmarshalling a given image. Finally a
	// single error is returned which may indicate a failure in the operation itself (e.g. invalid arguments)
	// or a failure code from the runtime. Even if the final error is not nil, there may still be some
	// inspected images returned.
	InspectImages(ctx context.Context, options InspectImagesOptions) ([]InspectedImage, error)
}

// BuildImage command types

type BuildSecretType string

const (
	EnvSecret  BuildSecretType = "env"
	FileSecret BuildSecretType = "file"
)

// ContainerBuildSecret is a secret made available to the image builder.
type ContainerBuildSecret struct {
	// The type of secret (defaults to file).
	Type BuildSecretType `json:"type,omitempty"`

	// The ID of the secret.
	ID string `json:"id"`

	// For file secrets, the source filepath of the secret; for env secrets, the environment
	// variable name. Required for file secrets, optional for env secrets (defaults to the ID).
	Source string `json:"source,omitempty"`

	// Only used for env secrets. If set, this value is applied via the configured environment
	// variable to the build command. If unset, the value comes from the ambient environment.
	Value string `json:"value,omitempty"`
}

// ContainerBuildContext describes how to build a container image from source.
type ContainerBuildContext struct {
	// The path to the directory to be used as the root of the build context.
	Context string `json:"context"`

	// A tar archive to stream to the image builder as the build context.
	ContextArchive *ContainerBuildContextArchive `json:"contextArchive,omitempty"`

	// The path to a Dockerfile to use for the build.
	Dockerfile string `json:"dockerfile,omitempty"`

	// Additional tags to apply to the image.
	Tags []string `json:"tags,omitempty"`

	// Additional --build-arg values to pass to the build command.
	Args []EnvVar `json:"args,omitempty"`

	// Build time secrets to be passed in to the builder via --secret.
	Secrets []ContainerBuildSecret `json:"secrets,omitempty"`

	// Optional: the name of the build stage to use for the build.
	Stage string `json:"stage,omitempty"`

	// Labels to apply to the built image.
	Labels []Label `json:"labels,omitempty"`

	// Optional target platform for the build (e.g. "linux/amd64").
	Platform string `json:"platform,omitempty"`
}

type ContainerBuildContextArchive struct {
	Digest      string `json:"digest"`
	Source      string `json:"source,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	RawContents string `json:"rawContents,omitempty"`
}

type BuildImageOptions struct {
	IidFile string
	Pull    bool

	*ContainerBuildContext

	StreamCommandOptions
	TimeoutOption
}

type BuildImage interface {
	// Build a new container image.
	BuildImage(ctx context.Context, options BuildImageOptions) error
}

// PullImage command types

type PullImageOptions struct {
	// ID of the image (name + tag)
	Image string `json:"Image"`

	// Digest of the image to pull (optional)
	Digest string `json:"Digest,omitempty"`

	TimeoutOption
}

type PullImage interface {
	// PullImage pulls a container image from a registry. If successful, the ID of the image is returned.
	PullImage(ctx context.Context, options PullImageOptions) (string, error)
}

// RemoveImages command types

type RemoveImagesOptions struct {
	// The list of image IDs or names to remove.
	Images []string

	// Force removal of the images.
	Force bool
}

type RemoveImages interface {
	// Removes images identified by the given list of names or IDs.
	// Returns the requested names or IDs that were successfully removed. If some images cannot be
	// removed, an error is reported, but images that can be removed are still processed.
	RemoveImages(ctx context.Context, options RemoveImagesOptions) ([]string, error)
}

// RemoveImagesSequentially applies a runtime-specific single-image removal operation while
// preserving the partial-success contract shared by the orchestrator removal APIs.
// Image removal output of the underlying runtime (e.g., Docker) tends to report affected tags and layers
// rather than requested identifiers, so separate exit results are required to attribute success reliably.
func RemoveImagesSequentially(
	ctx context.Context,
	options RemoveImagesOptions,
	removeImage func(ctx context.Context, image string, force bool) error,
) ([]string, error) {
	if len(options.Images) == 0 {
		return nil, fmt.Errorf("must specify at least one image")
	}
	if removeImage == nil {
		return nil, fmt.Errorf("image removal function cannot be nil")
	}

	removed := make([]string, 0, len(options.Images))
	var removalErrors error

	for _, image := range options.Images {
		if ctxErr := ctx.Err(); ctxErr != nil {
			removalErrors = errors.Join(removalErrors, ctxErr)
			break
		}

		if removeErr := removeImage(ctx, image, options.Force); removeErr != nil {
			removalErrors = errors.Join(removalErrors, fmt.Errorf("removing image %q: %w", image, removeErr))
			continue
		}

		removed = append(removed, image)
	}

	if len(removed) < len(options.Images) {
		removalErrors = errors.Join(
			removalErrors,
			errors.Join(
				ErrIncomplete,
				fmt.Errorf("only %d out of %d images were successfully removed", len(removed), len(options.Images)),
			),
		)
	}

	return removed, removalErrors
}

type ImageOrchestrator interface {
	InspectImages
	BuildImage
	PullImage
	RemoveImages

	RuntimeStatusChecker
}
