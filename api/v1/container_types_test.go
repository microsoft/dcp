/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestImageLayerValidate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		layer       ImageLayer
		expectError bool
		errorFields []string
	}{
		{
			name: "valid layer with source and sha256",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
				SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
			expectError: false,
		},
		{
			name: "valid layer with rawContents",
			layer: ImageLayer{
				Digest:      "abc123",
				RawContents: "dGVzdA==",
			},
			expectError: false,
		},
		{
			name: "missing digest",
			layer: ImageLayer{
				Source: "/path/to/layer.tar",
				SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
			expectError: true,
			errorFields: []string{"digest"},
		},
		{
			name: "missing source and rawContents",
			layer: ImageLayer{
				Digest: "abc123",
			},
			expectError: true,
		},
		{
			name: "both source and rawContents set",
			layer: ImageLayer{
				Digest:      "abc123",
				Source:      "/path/to/layer.tar",
				SHA256:      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				RawContents: "dGVzdA==",
			},
			expectError: true,
			errorFields: []string{"rawContents"},
		},
		{
			name: "source without sha256",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
			},
			expectError: true,
			errorFields: []string{"sha256"},
		},
		{
			name: "invalid sha256 - too short",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
				SHA256: "deadbeef",
			},
			expectError: true,
			errorFields: []string{"sha256"},
		},
		{
			name: "invalid sha256 - non-hex characters",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
				SHA256: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			},
			expectError: true,
			errorFields: []string{"sha256"},
		},
		{
			name: "valid sha256 with sha256: prefix",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
				SHA256: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
			expectError: false,
		},
		{
			name: "valid sha256 uppercase",
			layer: ImageLayer{
				Digest: "abc123",
				Source: "/path/to/layer.tar",
				SHA256: "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855",
			},
			expectError: false,
		},
		{
			name: "invalid rawContents - not valid base64",
			layer: ImageLayer{
				Digest:      "abc123",
				RawContents: "not-valid-base64!!!",
			},
			expectError: true,
			errorFields: []string{"rawContents"},
		},
		{
			name: "sha256 with rawContents is forbidden",
			layer: ImageLayer{
				Digest:      "abc123",
				RawContents: "dGVzdA==",
				SHA256:      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
			expectError: true,
			errorFields: []string{"sha256"},
		},
		{
			name: "sha256 without source is forbidden",
			layer: ImageLayer{
				Digest: "abc123",
				SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
			expectError: true,
			errorFields: []string{"sha256"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := tc.layer.Validate(field.NewPath("test"))

			if tc.expectError {
				require.NotEmpty(t, errs, "expected validation errors")
				for _, expectedField := range tc.errorFields {
					found := false
					for _, err := range errs {
						if err.Field == "test."+expectedField {
							found = true
							break
						}
					}
					assert.True(t, found, "expected error for field %q", expectedField)
				}
			} else {
				require.Empty(t, errs, "expected no validation errors, got: %v", errs)
			}
		})
	}
}

func TestImageLayerEqual(t *testing.T) {
	t.Parallel()

	layer1 := ImageLayer{
		Digest: "abc123",
		Source: "/path/to/layer.tar",
		SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}

	layer2 := ImageLayer{
		Digest: "abc123",
		Source: "/path/to/layer.tar",
		SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}

	layer3 := ImageLayer{
		Digest: "different",
		Source: "/path/to/layer.tar",
		SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}

	assert.True(t, layer1.Equal(&layer2), "identical layers should be equal")
	assert.False(t, layer1.Equal(&layer3), "layers with different digests should not be equal")
	assert.True(t, layer1.Equal(&layer1), "layer should be equal to itself")
	assert.False(t, layer1.Equal(nil), "layer should not be equal to nil")

	var nilLayer *ImageLayer
	assert.True(t, nilLayer.Equal(nil), "two nil layers should be equal")
	assert.False(t, nilLayer.Equal(&layer1), "nil layer should not be equal to non-nil layer")
}

func TestGetLifecycleKeyIncludesImageLayers(t *testing.T) {
	t.Parallel()

	specWithoutLayers := ContainerSpec{
		Image: "test:latest",
	}

	specWithLayers := ContainerSpec{
		Image: "test:latest",
		ImageLayers: []ImageLayer{
			{
				Digest:      "layer1",
				RawContents: "dGVzdA==",
			},
		},
	}

	specWithDifferentLayers := ContainerSpec{
		Image: "test:latest",
		ImageLayers: []ImageLayer{
			{
				Digest:      "layer2",
				RawContents: "dGVzdA==",
			},
		},
	}

	keyNoLayers, _, errNoLayers := specWithoutLayers.GetLifecycleKey()
	require.NoError(t, errNoLayers)

	keyWithLayers, _, errWithLayers := specWithLayers.GetLifecycleKey()
	require.NoError(t, errWithLayers)

	keyDifferentLayers, _, errDifferentLayers := specWithDifferentLayers.GetLifecycleKey()
	require.NoError(t, errDifferentLayers)

	assert.NotEqual(t, keyNoLayers, keyWithLayers, "lifecycle key should differ when layers are added")
	assert.NotEqual(t, keyWithLayers, keyDifferentLayers, "lifecycle key should differ when layer digests differ")
}
