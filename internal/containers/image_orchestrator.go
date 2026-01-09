// Copyright (c) Microsoft Corporation. All rights reserved.

package containers

import (
	"context"

	apiv1 "github.com/microsoft/dcp/api/v1"
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

type BuildImageOptions struct {
	IidFile string
	Pull    bool

	*apiv1.ContainerBuildContext

	StreamCommandOptions
	TimeoutOption
}

type BuildImage interface {
	// Build a new container image. If successful, the ID of the image is returned.
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

type ImageOrchestrator interface {
	InspectImages
	BuildImage
	PullImage

	RuntimeStatusChecker
}
