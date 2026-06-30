/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestContainerGetLeaseKey(t *testing.T) {
	t.Parallel()

	container := &Container{}
	container.Spec.ContainerName = " api "

	require.Equal(t, "containers/api", container.GetLeaseKey())
}

func TestContainerNetworkGetLeaseKey(t *testing.T) {
	t.Parallel()

	network := &ContainerNetwork{}
	network.Spec.NetworkName = " app-network "

	require.Equal(t, "containernetworks/app-network", network.GetLeaseKey())
}

func TestContainerSpecGetLifecycleKeyIncludesMonitorFields(t *testing.T) {
	t.Parallel()

	monitorPID := int64(12345)
	monitorTimestamp := metav1.NewMicroTime(time.Now().UTC())
	spec := &ContainerSpec{
		Image:         "api:dev",
		ContainerName: "api",
		Persistent:    true,
	}

	key, _, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)

	spec.MonitorPID = &monitorPID
	spec.MonitorTimestamp = monitorTimestamp
	keyWithMonitor, _, monitorKeyErr := spec.GetLifecycleKey()
	require.NoError(t, monitorKeyErr)
	require.NotEqual(t, key, keyWithMonitor)

	differentMonitorPID := monitorPID + 1
	spec.MonitorPID = &differentMonitorPID
	keyWithDifferentMonitorPID, _, differentMonitorPIDErr := spec.GetLifecycleKey()
	require.NoError(t, differentMonitorPIDErr)
	require.NotEqual(t, keyWithMonitor, keyWithDifferentMonitorPID)

	spec.MonitorPID = &monitorPID
	spec.MonitorTimestamp = metav1.NewMicroTime(monitorTimestamp.Time.Add(time.Second))
	keyWithDifferentMonitorTimestamp, _, differentMonitorTimestampErr := spec.GetLifecycleKey()
	require.NoError(t, differentMonitorTimestampErr)
	require.NotEqual(t, keyWithMonitor, keyWithDifferentMonitorTimestamp)
}

func TestContainerValidateMonitorFields(t *testing.T) {
	t.Parallel()

	monitorPID := int64(12345)
	negativeMonitorPID := int64(-1)
	monitorTimestamp := metav1.NewMicroTime(time.Now().UTC())

	testCases := []struct {
		name        string
		spec        ContainerSpec
		expectError bool
	}{
		{
			name: "valid persistent monitor",
			spec: ContainerSpec{
				Image:            "api:dev",
				ContainerName:    "api",
				Persistent:       true,
				MonitorPID:       &monitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
		},
		{
			name: "monitor requires persistent",
			spec: ContainerSpec{
				Image:            "api:dev",
				MonitorPID:       &monitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
		{
			name: "monitor pid requires timestamp",
			spec: ContainerSpec{
				Image:         "api:dev",
				ContainerName: "api",
				Persistent:    true,
				MonitorPID:    &monitorPID,
			},
			expectError: true,
		},
		{
			name: "monitor timestamp requires pid",
			spec: ContainerSpec{
				Image:            "api:dev",
				ContainerName:    "api",
				Persistent:       true,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
		{
			name: "monitor pid must be positive",
			spec: ContainerSpec{
				Image:            "api:dev",
				ContainerName:    "api",
				Persistent:       true,
				MonitorPID:       &negativeMonitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			container := &Container{Spec: testCase.spec}
			errors := container.Validate(nil)

			if testCase.expectError {
				require.NotEmpty(t, errors)
			} else {
				require.Empty(t, errors)
			}
		})
	}
}

func TestContainerValidateTerminalPersistentCombination(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		spec        ContainerSpec
		expectError bool
	}{
		{
			name: "non-persistent with terminal is allowed",
			spec: ContainerSpec{
				Image: "api:dev",
				Terminal: &TerminalSpec{
					UDSPath: "/tmp/api.sock",
					Cols:    80,
					Rows:    24,
				},
			},
		},
		{
			name: "persistent without terminal is allowed",
			spec: ContainerSpec{
				Image:         "api:dev",
				ContainerName: "api",
				Persistent:    true,
			},
		},
		{
			name: "persistent with terminal is rejected",
			spec: ContainerSpec{
				Image:         "api:dev",
				ContainerName: "api",
				Persistent:    true,
				Terminal: &TerminalSpec{
					UDSPath: "/tmp/api.sock",
					Cols:    80,
					Rows:    24,
				},
			},
			expectError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			container := &Container{Spec: testCase.spec}
			errors := container.Validate(nil)

			if testCase.expectError {
				require.NotEmpty(t, errors)
			} else {
				require.Empty(t, errors)
			}
		})
	}
}

func TestContainerValidateImageOnlyRequiredForCreationModes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		spec        ContainerSpec
		expectError bool
	}{
		{
			name:        "session mode requires image or build",
			spec:        ContainerSpec{},
			expectError: true,
		},
		{
			name: "persistent mode requires image or build",
			spec: ContainerSpec{
				ContainerName: "api",
				Mode:          ContainerModePersistent,
			},
			expectError: true,
		},
		{
			name: "persistent override requires image or build",
			spec: ContainerSpec{
				ContainerName: "api",
				Persistent:    true,
			},
			expectError: true,
		},
		{
			name: "existing mode does not require image or build",
			spec: ContainerSpec{
				ContainerName: "api",
				Mode:          ContainerModeExisting,
			},
		},
		{
			name: "cleanup mode does not require image or build",
			spec: ContainerSpec{
				ContainerName: "api",
				Mode:          ContainerModeCleanup,
			},
		},
		{
			name: "cleanup mode still validates explicit build",
			spec: ContainerSpec{
				ContainerName: "api",
				Mode:          ContainerModeCleanup,
				Build:         &ContainerBuildContext{},
			},
			expectError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			container := &Container{Spec: testCase.spec}
			errors := container.Validate(nil)

			if testCase.expectError {
				require.NotEmpty(t, errors)
			} else {
				require.Empty(t, errors)
			}
		})
	}
}

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

func TestContainerBuildContextEqual(t *testing.T) {
	t.Parallel()

	build1 := ContainerBuildContext{
		Context:    "/path/to/context",
		Dockerfile: "Dockerfile",
		Stage:      "runner",
		Platform:   "linux/amd64",
	}

	build2 := ContainerBuildContext{
		Context:    "/path/to/context",
		Dockerfile: "Dockerfile",
		Stage:      "runner",
		Platform:   "linux/amd64",
	}

	buildDifferentPlatform := ContainerBuildContext{
		Context:    "/path/to/context",
		Dockerfile: "Dockerfile",
		Stage:      "runner",
		Platform:   "linux/arm64",
	}

	buildNoPlatform := ContainerBuildContext{
		Context:    "/path/to/context",
		Dockerfile: "Dockerfile",
		Stage:      "runner",
	}

	assert.True(t, build1.Equal(&build2), "identical build contexts should be equal")
	assert.False(t, build1.Equal(&buildDifferentPlatform), "build contexts with different platforms should not be equal")
	assert.False(t, build1.Equal(&buildNoPlatform), "build context with platform should not equal one without")
	assert.True(t, build1.Equal(&build1), "build context should be equal to itself")
	assert.False(t, build1.Equal(nil), "build context should not be equal to nil")

	var nilBuild *ContainerBuildContext
	assert.True(t, nilBuild.Equal(nil), "two nil build contexts should be equal")
	assert.False(t, nilBuild.Equal(&build1), "nil build context should not be equal to non-nil")
}

func TestGetLifecycleKeyIncludesBuildPlatform(t *testing.T) {
	t.Parallel()

	specNoPlatform := ContainerSpec{
		Build: &ContainerBuildContext{
			Context:    "/nonexistent/context",
			Dockerfile: "Dockerfile",
		},
	}

	specAmd64 := ContainerSpec{
		Build: &ContainerBuildContext{
			Context:    "/nonexistent/context",
			Dockerfile: "Dockerfile",
			Platform:   "linux/amd64",
		},
	}

	specArm64 := ContainerSpec{
		Build: &ContainerBuildContext{
			Context:    "/nonexistent/context",
			Dockerfile: "Dockerfile",
			Platform:   "linux/arm64",
		},
	}

	keyNoPlatform, _, errNoPlatform := specNoPlatform.GetLifecycleKey()
	require.NoError(t, errNoPlatform)

	keyAmd64, _, errAmd64 := specAmd64.GetLifecycleKey()
	require.NoError(t, errAmd64)

	keyArm64, _, errArm64 := specArm64.GetLifecycleKey()
	require.NoError(t, errArm64)

	assert.NotEqual(t, keyNoPlatform, keyAmd64, "lifecycle key should differ when platform is added")
	assert.NotEqual(t, keyAmd64, keyArm64, "lifecycle key should differ between platforms")
}

func TestGetLifecycleKeyDisambiguatesStageAndPlatform(t *testing.T) {
	t.Parallel()

	// Without length framing, (Stage="ab", Platform="c") and
	// (Stage="a", Platform="bc") would hash identically.
	specA := ContainerSpec{
		Build: &ContainerBuildContext{
			Context:    "/nonexistent/context",
			Dockerfile: "Dockerfile",
			Stage:      "ab",
			Platform:   "c",
		},
	}

	specB := ContainerSpec{
		Build: &ContainerBuildContext{
			Context:    "/nonexistent/context",
			Dockerfile: "Dockerfile",
			Stage:      "a",
			Platform:   "bc",
		},
	}

	keyA, _, errA := specA.GetLifecycleKey()
	require.NoError(t, errA)

	keyB, _, errB := specB.GetLifecycleKey()
	require.NoError(t, errB)

	assert.NotEqual(t, keyA, keyB, "stage/platform boundary must be unambiguous")
}
