/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that LogicalContainer.GetLogSource exposes only the observed runtime source,
// returning nil until one is recorded rather than substituting startup output.
func TestLogicalContainerGetLogSource(t *testing.T) {
	t.Parallel()
	container := &LogicalContainer{
		Status: LogicalContainerStatus{
			StartupLogs: &LogSource{Name: "image", UID: "image-uid"},
		},
	}
	require.Nil(t, container.GetLogSource())
	container.Status.RuntimeLogs = &LogSource{Name: "physical", UID: "physical-uid", ContainerID: "runtime-id"}
	require.Same(t, container.Status.RuntimeLogs, container.GetLogSource())
}

// Verifies that LogicalContainerMode exposes creation and reuse policy consistently with its defaults.
func TestLogicalContainerModePolicies(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		mode   LogicalContainerMode
		reuse  bool
		create bool
	}{
		{mode: "", create: true},
		{mode: LogicalContainerModeSession, create: true},
		{mode: LogicalContainerModePersistent, reuse: true, create: true},
		{mode: LogicalContainerModeExisting, reuse: true},
	} {
		mode := (LogicalContainerSpec{Mode: testCase.mode}).EffectiveMode()
		require.Equal(t, testCase.reuse, mode.ShouldReuseExisting(), "mode %q", testCase.mode)
		require.Equal(t, testCase.create, mode.ShouldCreateIfMissing(), "mode %q", testCase.mode)
	}
}

// Verifies that LogicalContainerSpec.GetLifecycleKey honors explicit keys, normalizes declared
// configuration without mutating it, and changes the computed key when runtime configuration changes.
func TestLogicalContainerSpecGetLifecycleKey(t *testing.T) {
	t.Parallel()
	original := &LogicalContainerSpec{
		Mode: LogicalContainerModePersistent, ContainerName: "runtime-container",
		Container: &LogicalContainerConfig{
			ImageRef: "image", Command: []string{"run"},
			Env:          []commonapi.EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}},
			Labels:       []commonapi.Label{{Key: "b", Value: "2"}, {Key: "a", Value: "1"}},
			VolumeMounts: []VolumeMount{{Type: NamedVolumeMount, VolumeRef: "volume", Target: "/data"}},
			Networks:     []LogicalContainerNetworkConnectionConfig{{NetworkRef: "network", Aliases: []string{"second", "first"}}},
			Ports:        []ContainerPort{{ContainerPort: 8080}},
		},
	}
	unchanged := original.DeepCopy()
	key, computed, keyErr := original.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.True(t, computed)
	require.Equal(t, unchanged, original)

	equivalent := original.DeepCopy()
	equivalent.ContainerName = "another-runtime-container"
	equivalent.Mode, equivalent.Stop = LogicalContainerModeSession, true
	equivalent.Container.ImageRef = "test-namespace/image"
	equivalent.Container.VolumeMounts[0].VolumeRef = "test-namespace/volume"
	equivalent.Container.Networks[0].NetworkRef = "test-namespace/network"
	slices.Reverse(equivalent.Container.Env)
	slices.Reverse(equivalent.Container.Labels)
	slices.Reverse(equivalent.Container.Networks[0].Aliases)
	equivalent.Container.Ports[0].Protocol = commonapi.PortProtocolTCP
	equivalent.Container.Ports[0].RangeSize = 1
	equivalentKey, _, equivalentErr := equivalent.GetLifecycleKey()
	require.NoError(t, equivalentErr)
	require.Equal(t, key, equivalentKey)

	changed := original.DeepCopy()
	changed.Container.Command = []string{"different"}
	changedKey, _, changedErr := changed.GetLifecycleKey()
	require.NoError(t, changedErr)
	require.NotEqual(t, key, changedKey)

	explicit := &LogicalContainerSpec{LifecycleKey: "user-key"}
	explicitKey, explicitComputed, explicitErr := explicit.GetLifecycleKey()
	require.NoError(t, explicitErr)
	require.False(t, explicitComputed)
	require.Equal(t, "user-key", explicitKey)
}

// Verifies that LogicalContainerSpec.GetLifecycleKey excludes dynamic network membership,
// ordering, and aliases without mutating the declared attachments.
func TestLogicalContainerSpecGetLifecycleKeyIgnoresNetworks(t *testing.T) {
	t.Parallel()
	original := &LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"}}
	key, _, keyErr := original.GetLifecycleKey()
	require.NoError(t, keyErr)
	for _, testCase := range []struct {
		name     string
		networks []LogicalContainerNetworkConnectionConfig
	}{
		{name: "nil"},
		{name: "empty", networks: []LogicalContainerNetworkConnectionConfig{}},
		{name: "connected", networks: []LogicalContainerNetworkConnectionConfig{
			{NetworkRef: "first", Aliases: []string{"app"}},
		}},
		{name: "additional attachment", networks: []LogicalContainerNetworkConnectionConfig{
			{NetworkRef: "first", Aliases: []string{"app"}},
			{NetworkRef: "second", Aliases: []string{"service"}},
		}},
		{name: "reordered attachments", networks: []LogicalContainerNetworkConnectionConfig{
			{NetworkRef: "second", Aliases: []string{"service"}},
			{NetworkRef: "first", Aliases: []string{"app"}},
		}},
		{name: "changed aliases", networks: []LogicalContainerNetworkConnectionConfig{
			{NetworkRef: "first", Aliases: []string{"second", "first"}},
		}},
		{name: "qualified reference", networks: []LogicalContainerNetworkConnectionConfig{
			{NetworkRef: "namespace/first"},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := original.DeepCopy()
			changed.Container.Networks = testCase.networks
			before := changed.DeepCopy()
			changedKey, _, changedErr := changed.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.Equal(t, key, changedKey)
			require.Equal(t, before, changed)
		})
	}
}

// Verifies that LogicalContainerSpec.GetLifecycleKey ignores collection order using V1's sort keys,
// preserves the supplied configuration, and still distinguishes configuration changes.
func TestLogicalContainerSpecGetLifecycleKeyMatchesV1CollectionOrdering(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		container *LogicalContainerConfig
		reorder   func(*LogicalContainerConfig)
		change    func(*LogicalContainerConfig)
	}{
		{
			name: "ports",
			container: &LogicalContainerConfig{ImageRef: "image", Ports: []ContainerPort{
				{Protocol: commonapi.PortProtocolUDP, HostPort: 8000, ContainerPort: 80},
				{Protocol: commonapi.PortProtocolTCP, HostPort: 8001, ContainerPort: 81},
				{HostPort: 8000, ContainerPort: 80},
			}},
			reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.Ports) },
			change:  func(config *LogicalContainerConfig) { config.Ports[0].ContainerPort++ },
		},
		{
			name: "mounts",
			container: &LogicalContainerConfig{ImageRef: "image", VolumeMounts: []VolumeMount{
				{Type: BindMount, Source: "/source-second", Target: "/second"},
				{Type: NamedVolumeMount, VolumeRef: "namespace/volume", Target: "/first"},
			}},
			reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.VolumeMounts) },
			change:  func(config *LogicalContainerConfig) { config.VolumeMounts[0].ReadOnly = true },
		},
		{
			name: "files",
			container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{
				{Destination: "/second", Entries: []FileSystemEntry{{Name: "config", Contents: "second-value"}}},
				{Destination: "/first", Entries: []FileSystemEntry{{Name: "config", Contents: "first-value"}}},
			}},
			reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.CreateFiles) },
			change:  func(config *LogicalContainerConfig) { config.CreateFiles[0].Entries[0].Contents = "changed" },
		},
		{
			name: "secrets",
			container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
				Context: "/source", Digest: "source-digest",
				Secrets: []ContainerBuildSecret{
					{Type: EnvSecret, ID: "second", Value: "second-value"},
					{Type: EnvSecret, ID: "first", Value: "first-value"},
				},
			}}},
			reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.Image.Build.Secrets) },
			change:  func(config *LogicalContainerConfig) { config.Image.Build.Secrets[0].Value = "changed" },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			original := &LogicalContainerSpec{Container: testCase.container}
			before := original.DeepCopy()
			key, _, keyErr := original.GetLifecycleKey()
			require.NoError(t, keyErr)
			require.Equal(t, before, original)
			equivalent := original.DeepCopy()
			testCase.reorder(equivalent.Container)
			equivalentBefore := equivalent.DeepCopy()
			equivalentKey, _, equivalentErr := equivalent.GetLifecycleKey()
			require.NoError(t, equivalentErr)
			require.Equal(t, key, equivalentKey)
			require.Equal(t, equivalentBefore, equivalent)
			testCase.change(equivalent.Container)
			changedKey, _, changedErr := equivalent.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.NotEqual(t, key, changedKey)
		})
	}
}

// Verifies that LogicalContainerSpec.GetLifecycleKey preserves command and file-entry order
// and the declaration order of entries sharing V1's port, mount, file, or secret sort keys.
func TestLogicalContainerSpecGetLifecycleKeyPreservesMeaningfulOrder(t *testing.T) {
	t.Parallel()
	original := &LogicalContainerSpec{Container: &LogicalContainerConfig{
		Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
			Context: "/source", Digest: "source-digest",
			Secrets: []ContainerBuildSecret{
				{Type: EnvSecret, ID: "same", Value: "first"},
				{Type: EnvSecret, ID: "same", Value: "second"},
			},
		}},
		Command: []string{"first", "second"},
		Ports:   []ContainerPort{{ContainerPort: 80}, {ContainerPort: 81}},
		VolumeMounts: []VolumeMount{
			{Type: BindMount, Source: "/first", Target: "/same"},
			{Type: BindMount, Source: "/second", Target: "/same"},
		},
		CreateFiles: []CreateFileSystem{
			{Destination: "/same", Entries: []FileSystemEntry{{Name: "first"}, {Name: "second"}}},
			{Destination: "/same", Entries: []FileSystemEntry{{Name: "third"}}},
		},
	}}
	key, _, keyErr := original.GetLifecycleKey()
	require.NoError(t, keyErr)
	for _, testCase := range []struct {
		name    string
		reorder func(*LogicalContainerConfig)
	}{
		{name: "command", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.Command) }},
		{name: "file entries", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.CreateFiles[0].Entries) }},
		{name: "equal port keys", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.Ports) }},
		{name: "equal mount targets", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.VolumeMounts) }},
		{name: "equal file destinations", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.CreateFiles) }},
		{name: "equal secret IDs", reorder: func(config *LogicalContainerConfig) { slices.Reverse(config.Image.Build.Secrets) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := original.DeepCopy()
			testCase.reorder(changed.Container)
			changedKey, _, changedErr := changed.GetLifecycleKey()
			require.NoError(t, changedErr)
			require.NotEqual(t, key, changedKey)
		})
	}
}

// Verifies that LogicalContainerSpec.GetLifecycleKey distinguishes different duplicate precedence
// in environment variables, container labels, and build labels.
func TestLogicalContainerSpecGetLifecycleKeyPreservesDuplicatePrecedence(t *testing.T) {
	t.Parallel()
	// Unstable key sorting normalized these orders identically despite opposite duplicate precedence.
	firstOrder := []commonapi.Label{
		{Key: "label00", Value: "unchanged"}, {Key: "label02", Value: "unchanged"},
		{Key: "label13", Value: "unchanged"}, {Key: "label09", Value: "unchanged"},
		{Key: "duplicate", Value: "second"}, {Key: "label03", Value: "unchanged"},
		{Key: "label07", Value: "unchanged"}, {Key: "label06", Value: "unchanged"},
		{Key: "label10", Value: "unchanged"}, {Key: "label04", Value: "unchanged"},
		{Key: "label12", Value: "unchanged"}, {Key: "label11", Value: "unchanged"},
		{Key: "duplicate", Value: "first"}, {Key: "label05", Value: "unchanged"},
		{Key: "label08", Value: "unchanged"}, {Key: "label01", Value: "unchanged"},
	}
	secondOrder := []commonapi.Label{
		{Key: "label09", Value: "unchanged"}, {Key: "duplicate", Value: "first"},
		{Key: "label10", Value: "unchanged"}, {Key: "label05", Value: "unchanged"},
		{Key: "label13", Value: "unchanged"}, {Key: "label07", Value: "unchanged"},
		{Key: "label03", Value: "unchanged"}, {Key: "label01", Value: "unchanged"},
		{Key: "label02", Value: "unchanged"}, {Key: "label06", Value: "unchanged"},
		{Key: "label00", Value: "unchanged"}, {Key: "duplicate", Value: "second"},
		{Key: "label11", Value: "unchanged"}, {Key: "label08", Value: "unchanged"},
		{Key: "label04", Value: "unchanged"}, {Key: "label12", Value: "unchanged"},
	}
	original := &LogicalContainerSpec{Container: &LogicalContainerConfig{
		Labels: firstOrder,
		Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
			Context: "/source", Labels: firstOrder,
		}},
	}}
	for _, label := range firstOrder {
		original.Container.Env = append(original.Container.Env, commonapi.EnvVar{Name: label.Key, Value: label.Value})
	}
	key, _, keyErr := original.GetLifecycleKey()
	require.NoError(t, keyErr)

	changedEnv := original.DeepCopy()
	for index, label := range secondOrder {
		changedEnv.Container.Env[index] = commonapi.EnvVar{Name: label.Key, Value: label.Value}
	}
	envKey, _, envErr := changedEnv.GetLifecycleKey()
	require.NoError(t, envErr)
	require.NotEqual(t, key, envKey)

	changedLabels := original.DeepCopy()
	changedLabels.Container.Labels = secondOrder
	labelKey, _, labelErr := changedLabels.GetLifecycleKey()
	require.NoError(t, labelErr)
	require.NotEqual(t, key, labelKey)

	changedBuildLabels := original.DeepCopy()
	changedBuildLabels.Container.Image.Build.Labels = secondOrder
	buildLabelKey, _, buildLabelErr := changedBuildLabels.GetLifecycleKey()
	require.NoError(t, buildLabelErr)
	require.NotEqual(t, key, buildLabelKey)
}

// Verifies that LogicalContainerSpec.GetLifecycleKey uses the caller-supplied build digest,
// not archive transport or bytes, and does not need Dockerfiles or file secrets to exist.
func TestLogicalContainerSpecGetLifecycleKeyUsesSuppliedBuildDigest(t *testing.T) {
	t.Parallel()
	original := &LogicalContainerSpec{Container: &LogicalContainerConfig{
		Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{
			ContextArchive: &ContainerBuildContextArchive{RawContents: "Zmlyc3Q="},
			Dockerfile:     "Dockerfile",
			Secrets:        []ContainerBuildSecret{{Type: FileSecret, ID: "secret", Source: filepath.Join(t.TempDir(), "missing-secret")}},
		}},
	}}
	keyWithoutDigest, _, keyErr := original.GetLifecycleKey()
	require.NoError(t, keyErr)
	changedArchive := original.DeepCopy()
	changedArchive.Container.Image.Build.ContextArchive.RawContents = "c2Vjb25k"
	archiveKey, _, archiveErr := changedArchive.GetLifecycleKey()
	require.NoError(t, archiveErr)
	require.Equal(t, keyWithoutDigest, archiveKey)

	withDigest := original.DeepCopy()
	withDigest.Container.Image.Build.Digest = "first-digest"
	keyWithDigest, _, digestErr := withDigest.GetLifecycleKey()
	require.NoError(t, digestErr)
	require.NotEqual(t, keyWithoutDigest, keyWithDigest)
	withDigest.Container.Image.Build.Digest = "second-digest"
	keyWithChangedDigest, _, changedDigestErr := withDigest.GetLifecycleKey()
	require.NoError(t, changedDigestErr)
	require.NotEqual(t, keyWithDigest, keyWithChangedDigest)

	withDirectory := original.DeepCopy()
	withDirectory.Container.Image.Build.ContextArchive = nil
	withDirectory.Container.Image.Build.Context = t.TempDir()
	directoryBefore := withDirectory.DeepCopy()
	_, _, directoryErr := withDirectory.GetLifecycleKey()
	require.NoError(t, directoryErr)
	require.Equal(t, directoryBefore, withDirectory)
}

// Verifies that LogicalContainer.Validate enforces lifecycle modes, container presence, image source exclusivity, and runtime configuration validation.
func TestLogicalContainerValidate(t *testing.T) {
	negativeRetryLimit := int32(-1)
	invalidUmask := fs.ModeDir
	testCases := []struct {
		name           string
		spec           LogicalContainerSpec
		expectedFields []string
	}{
		{name: "default session", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"}}},
		{name: "explicit session", spec: LogicalContainerSpec{Mode: LogicalContainerModeSession, Container: &LogicalContainerConfig{ImageRef: "image"}}},
		{name: "persistent", spec: LogicalContainerSpec{Mode: LogicalContainerModePersistent, ContainerName: "persistent-container", LifecycleKey: "opaque-key", Container: &LogicalContainerConfig{ImageRef: "image"}}},
		{name: "existing by name", spec: LogicalContainerSpec{Mode: LogicalContainerModeExisting, ContainerName: "existing-container", Stop: true}},
		{name: "concrete image", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "example/image:latest"}}}},
		{name: "concrete build", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{Context: "/source"}}}}},
		{name: "tagged build", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "example/built-image:latest", Build: &ContainerBuildContext{Context: "/source"}}}}},
		{name: "qualified image reference", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "test-namespace/image"}}},
		{
			name: "runtime configuration",
			spec: LogicalContainerSpec{Container: &LogicalContainerConfig{
				ImageRef:   "image",
				Entrypoint: "/bin/app",
				Command:    []string{"--serve"},
				Env:        []commonapi.EnvVar{{Name: "PORT", Value: "8080"}},
				Ports:      []ContainerPort{{ContainerPort: 8080, RangeSize: 2}},
				VolumeMounts: []VolumeMount{
					{Type: BindMount, Source: "/host", Target: "/data"},
					{Type: NamedVolumeMount, VolumeRef: "test-namespace/volume", Target: "/cache"},
				},
				Networks:    []LogicalContainerNetworkConnectionConfig{{NetworkRef: "test-namespace/network", Aliases: []string{"app"}}},
				CreateFiles: []CreateFileSystem{{Destination: "/app", Entries: []FileSystemEntry{{Name: "config", Contents: "value"}}}},
				Labels:      []commonapi.Label{{Key: "app", Value: "example"}},
			}},
		},
		{name: "missing default session container", expectedFields: []string{"spec.container"}},
		{name: "missing session container", spec: LogicalContainerSpec{Mode: LogicalContainerModeSession}, expectedFields: []string{"spec.container"}},
		{name: "missing persistent container", spec: LogicalContainerSpec{Mode: LogicalContainerModePersistent, ContainerName: "persistent-container"}, expectedFields: []string{"spec.container"}},
		{name: "existing with container", spec: LogicalContainerSpec{Mode: LogicalContainerModeExisting, ContainerName: "existing-container", Container: &LogicalContainerConfig{ImageRef: "image"}}, expectedFields: []string{"spec.container"}},
		{name: "cleanup is unsupported", spec: LogicalContainerSpec{Mode: "cleanup", Container: &LogicalContainerConfig{ImageRef: "image"}}, expectedFields: []string{"spec.mode"}},
		{name: "unknown mode", spec: LogicalContainerSpec{Mode: "unknown", Container: &LogicalContainerConfig{ImageRef: "image"}}, expectedFields: []string{"spec.mode"}},
		{name: "persistent without name", spec: LogicalContainerSpec{Mode: LogicalContainerModePersistent, Container: &LogicalContainerConfig{ImageRef: "image"}}, expectedFields: []string{"spec.containerName"}},
		{name: "existing without name", spec: LogicalContainerSpec{Mode: LogicalContainerModeExisting}, expectedFields: []string{"spec.containerName"}},
		{name: "invalid runtime name", spec: LogicalContainerSpec{ContainerName: "invalid/name", Container: &LogicalContainerConfig{ImageRef: "image"}}, expectedFields: []string{"spec.containerName"}},
		{name: "missing image source", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{}}, expectedFields: []string{"spec.container"}},
		{name: "both image sources", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Image: &PhysicalContainerImageConfig{Image: "example/image"}}}, expectedFields: []string{"spec.container.image"}},
		{name: "invalid image reference", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "/"}}, expectedFields: []string{"spec.container.imageRef", "spec.container.imageRef", "spec.container.imageRef"}},
		{name: "cross namespace image", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "other-namespace/image"}}, expectedFields: []string{"spec.container.imageRef"}},
		{name: "empty concrete image", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{}}}, expectedFields: []string{"spec.container.image.image"}},
		{name: "invalid concrete image", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "image with spaces"}}}, expectedFields: []string{"spec.container.image.image"}},
		{name: "missing build context", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{}}}}, expectedFields: []string{"spec.container.image.build"}},
		{name: "conflicting build contexts", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Build: &ContainerBuildContext{Context: "/source", ContextArchive: &ContainerBuildContextArchive{RawContents: "dGFy"}}}}}, expectedFields: []string{"spec.container.image.build.contextArchive"}},
		{name: "invalid pull policy", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "image", PullPolicy: "invalid"}}}, expectedFields: []string{"spec.container.image.pullPolicy"}},
		{name: "build policy without build", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "image", BuildPolicy: BuildPolicyAlways}}}, expectedFields: []string{"spec.container.image.buildPolicy"}},
		{name: "negative retry limit", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{Image: &PhysicalContainerImageConfig{Image: "image", PullRetryLimit: &negativeRetryLimit}}}, expectedFields: []string{"spec.container.image.pullRetryLimit"}},
		{name: "invalid port", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Ports: []ContainerPort{{ContainerPort: 0}}}}, expectedFields: []string{"spec.container.ports[0].containerPort"}},
		{name: "cross namespace volume", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", VolumeMounts: []VolumeMount{{Type: NamedVolumeMount, VolumeRef: "other-namespace/volume", Target: "/data"}}}}, expectedFields: []string{"spec.container.volumeMounts[0].volumeRef"}},
		{name: "bind mount with volume reference", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", VolumeMounts: []VolumeMount{{Type: BindMount, Source: "/host", VolumeRef: "volume", Target: "/data"}}}}, expectedFields: []string{"spec.container.volumeMounts[0].volumeRef"}},
		{name: "cross namespace network", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Networks: []LogicalContainerNetworkConnectionConfig{{NetworkRef: "other-namespace/network"}}}}, expectedFields: []string{"spec.container.networks[0].networkRef"}},
		{name: "missing logical network reference", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Networks: []LogicalContainerNetworkConnectionConfig{{}}}}, expectedFields: []string{"spec.container.networks[0].networkRef"}},
		{name: "invalid logical network reference", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Networks: []LogicalContainerNetworkConnectionConfig{{NetworkRef: "Invalid_Network"}}}}, expectedFields: []string{"spec.container.networks[0].networkRef"}},
		{name: "missing label key", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", Labels: []commonapi.Label{{Value: "value"}}}}, expectedFields: []string{"spec.container.labels[0].key"}},
		{name: "relative file destination", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{{Destination: "relative", Entries: []FileSystemEntry{{Name: "file"}}}}}}, expectedFields: []string{"spec.container.createFiles[0].destination"}},
		{name: "empty file entries", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{{Destination: "/app"}}}}, expectedFields: []string{"spec.container.createFiles[0].entries"}},
		{name: "invalid file entry", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{{Destination: "/app", Entries: []FileSystemEntry{{Name: "nested/file"}}}}}}, expectedFields: []string{"spec.container.createFiles[0].entries[0].name"}},
		{name: "invalid file ownership", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{{Destination: "/app", DefaultOwner: -1, DefaultGroup: -1, Entries: []FileSystemEntry{{Name: "file"}}}}}}, expectedFields: []string{"spec.container.createFiles[0].defaultOwner", "spec.container.createFiles[0].defaultGroup"}},
		{name: "invalid file umask", spec: LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image", CreateFiles: []CreateFileSystem{{Destination: "/app", Umask: &invalidUmask, Entries: []FileSystemEntry{{Name: "file"}}}}}}, expectedFields: []string{"spec.container.createFiles[0].umask"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			container := &LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
				Spec:       testCase.spec,
			}
			errorList := container.Validate(context.Background())
			actualFields := make([]string, 0, len(errorList))
			for _, validationError := range errorList {
				actualFields = append(actualFields, validationError.Field)
			}
			require.ElementsMatch(t, testCase.expectedFields, actualFields, "%v", errorList)
		})
	}
}

// Verifies that LogicalContainerSpec.EffectiveMode defaults to session without changing explicit modes.
func TestLogicalContainerSpecEffectiveMode(t *testing.T) {
	require.Equal(t, LogicalContainerModeSession, (LogicalContainerSpec{}).EffectiveMode())
	for _, mode := range []LogicalContainerMode{LogicalContainerModeSession, LogicalContainerModePersistent, LogicalContainerModeExisting} {
		require.Equal(t, mode, (LogicalContainerSpec{Mode: mode}).EffectiveMode())
	}
}

// Verifies that LogicalContainer.Validate accepts monitor identities only for persistent mode and requires a valid PID/timestamp pair.
func TestLogicalContainerValidateMonitor(t *testing.T) {
	timestamp := metav1.NewMicroTime(time.Now().UTC())
	validPID := int64(42)
	zeroPID := int64(0)
	negativePID := int64(-1)
	oversizedPID := int64(1) << 32
	testCases := []struct {
		name           string
		mode           LogicalContainerMode
		pid            *int64
		timestamp      metav1.MicroTime
		expectedFields []string
	}{
		{name: "persistent monitor", mode: LogicalContainerModePersistent, pid: &validPID, timestamp: timestamp},
		{name: "persistent without monitor", mode: LogicalContainerModePersistent},
		{name: "session monitor", mode: LogicalContainerModeSession, pid: &validPID, timestamp: timestamp, expectedFields: []string{"spec.monitorPID", "spec.monitorTimestamp"}},
		{name: "existing monitor", mode: LogicalContainerModeExisting, pid: &validPID, timestamp: timestamp, expectedFields: []string{"spec.monitorPID", "spec.monitorTimestamp"}},
		{name: "zero pid", mode: LogicalContainerModePersistent, pid: &zeroPID, timestamp: timestamp, expectedFields: []string{"spec.monitorPID"}},
		{name: "negative pid", mode: LogicalContainerModePersistent, pid: &negativePID, timestamp: timestamp, expectedFields: []string{"spec.monitorPID"}},
		{name: "oversized pid", mode: LogicalContainerModePersistent, pid: &oversizedPID, timestamp: timestamp, expectedFields: []string{"spec.monitorPID"}},
		{name: "missing timestamp", mode: LogicalContainerModePersistent, pid: &validPID, expectedFields: []string{"spec.monitorTimestamp"}},
		{name: "missing pid", mode: LogicalContainerModePersistent, timestamp: timestamp, expectedFields: []string{"spec.monitorPID"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			container := &LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
				Spec: LogicalContainerSpec{
					Mode: testCase.mode, ContainerName: "runtime-container",
					MonitorPID: testCase.pid, MonitorTimestamp: testCase.timestamp,
				},
			}
			if testCase.mode != LogicalContainerModeExisting {
				container.Spec.Container = &LogicalContainerConfig{ImageRef: "image"}
			}
			errorList := container.Validate(context.Background())
			actualFields := make([]string, 0, len(errorList))
			for _, validationError := range errorList {
				actualFields = append(actualFields, validationError.Field)
			}
			require.ElementsMatch(t, testCase.expectedFields, actualFields, "%v", errorList)
		})
	}
}

// Verifies that LogicalContainer.Validate requires a valid namespace and rejects oversized annotations.
func TestLogicalContainerValidateMetadata(t *testing.T) {
	testCases := []struct {
		name          string
		namespace     string
		annotations   map[string]string
		expectedField string
	}{
		{name: "missing namespace", expectedField: "metadata.namespace"},
		{name: "invalid namespace", namespace: "Invalid_Namespace", expectedField: "metadata.namespace"},
		{name: "oversized annotations", namespace: "test-namespace", annotations: map[string]string{"large": strings.Repeat("a", commonapi.MaxAnnotationsTotalSize)}, expectedField: "metadata.annotations"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			container := &LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: testCase.namespace, Annotations: testCase.annotations},
				Spec:       LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"}},
			}
			errorList := container.Validate(context.Background())
			require.NotEmpty(t, errorList)
			require.Equal(t, testCase.expectedField, errorList[0].Field)
		})
	}
}

// Verifies that LogicalContainer.Validate rejects new resources during shutdown while allowing resources already being deleted.
func TestLogicalContainerValidateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)
	container := &LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
		Spec:       LogicalContainerSpec{Container: &LogicalContainerConfig{ImageRef: "image"}},
	}
	require.Contains(t, container.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	deletionTime := metav1.Now()
	container.DeletionTimestamp = &deletionTime
	require.Empty(t, container.Validate(context.Background()))
}

// Verifies that LogicalContainer.ValidateUpdate keeps the spec immutable except for a one-way stop request and allows status and metadata updates.
func TestLogicalContainerValidateUpdate(t *testing.T) {
	original := &LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
		Spec: LogicalContainerSpec{
			ContainerName: "runtime-container", Mode: LogicalContainerModePersistent, LifecycleKey: "key",
			Container: &LogicalContainerConfig{ImageRef: "image"},
		},
	}
	testCases := []struct {
		name          string
		change        func(*LogicalContainer)
		expectedField string
	}{
		{name: "unchanged", change: func(*LogicalContainer) {}},
		{name: "stop", change: func(container *LogicalContainer) { container.Spec.Stop = true }},
		{name: "status", change: func(container *LogicalContainer) { container.Status.Phase = LogicalContainerPhaseRunning }},
		{name: "annotations", change: func(container *LogicalContainer) { container.Annotations = map[string]string{"key": "value"} }},
		{name: "name", change: func(container *LogicalContainer) { container.Spec.ContainerName = "other-container" }, expectedField: "spec"},
		{name: "mode", change: func(container *LogicalContainer) { container.Spec.Mode = LogicalContainerModeSession }, expectedField: "spec"},
		{name: "lifecycle key", change: func(container *LogicalContainer) { container.Spec.LifecycleKey = "other-key" }, expectedField: "spec"},
		{name: "remove container", change: func(container *LogicalContainer) { container.Spec.Container = nil }, expectedField: "spec"},
		{name: "image reference", change: func(container *LogicalContainer) { container.Spec.Container.ImageRef = "other-image" }, expectedField: "spec"},
		{name: "image source", change: func(container *LogicalContainer) {
			container.Spec.Container.ImageRef = ""
			container.Spec.Container.Image = &PhysicalContainerImageConfig{Image: "other-image"}
		}, expectedField: "spec"},
		{name: "command", change: func(container *LogicalContainer) { container.Spec.Container.Command = []string{"other-command"} }, expectedField: "spec"},
		{name: "environment", change: func(container *LogicalContainer) {
			container.Spec.Container.Env = []commonapi.EnvVar{{Name: "KEY", Value: "value"}}
		}, expectedField: "spec"},
		{name: "monitor", change: func(container *LogicalContainer) {
			monitorPID := int64(42)
			container.Spec.MonitorPID = &monitorPID
			container.Spec.MonitorTimestamp = metav1.NewMicroTime(time.Now().UTC())
		}, expectedField: "spec"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			updated := original.DeepCopy()
			testCase.change(updated)
			errorList := updated.ValidateUpdate(context.Background(), original)
			if testCase.expectedField == "" {
				require.Empty(t, errorList)
			} else {
				require.Len(t, errorList, 1)
				require.Equal(t, testCase.expectedField, errorList[0].Field)
			}
		})
	}
	stopped := original.DeepCopy()
	stopped.Spec.Stop = true
	require.Empty(t, stopped.DeepCopy().ValidateUpdate(context.Background(), stopped))
	errorList := original.ValidateUpdate(context.Background(), stopped)
	require.Len(t, errorList, 1)
	require.Equal(t, "spec.stop", errorList[0].Field)
}

// Verifies that LogicalContainer is registered as a namespaced V2 storage kind with a status subresource and list support.
func TestLogicalContainerRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	registered, newErr := scheme.New(GroupVersion.WithKind("LogicalContainer"))
	require.NoError(t, newErr)
	require.IsType(t, &LogicalContainer{}, registered)
	registeredList, listErr := scheme.New(GroupVersion.WithKind("LogicalContainerList"))
	require.NoError(t, listErr)
	require.IsType(t, &LogicalContainerList{}, registeredList)

	container := &LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"}}
	require.Contains(t, PersistentTypes, &LogicalContainer{})
	require.False(t, scheme.Recognizes(GroupVersion.WithKind("Container")))
	require.Equal(t, GroupVersion.WithResource("logicalcontainers"), container.GetGroupVersionResource())
	require.Equal(t, types.NamespacedName{Namespace: "test-namespace", Name: "container"}, container.NamespacedName())
	require.True(t, container.NamespaceScoped())
	require.True(t, container.IsStorageVersion())
	require.Equal(t, []string{"lctr"}, container.ShortNames())
	require.IsType(t, &LogicalContainer{}, container.New())
	require.IsType(t, &LogicalContainerList{}, container.NewList())
	require.Same(t, &container.ObjectMeta, container.GetObjectMeta())
	require.IsType(t, LogicalContainerStatus{}, container.GetStatus())

	list := &LogicalContainerList{Items: []LogicalContainer{*container}}
	require.Same(t, &list.ListMeta, list.GetListMeta())
	require.Equal(t, uint32(1), list.ItemCount())
	require.Same(t, &list.Items[0], list.GetItems()[0])
}

// Verifies that LogicalContainer.GetLeaseKey uses runtime identity rather than API identity,
// sharing the same container lease namespace across V1/V2 resources and API namespaces.
func TestLogicalContainerGetLeaseKey(t *testing.T) {
	first := &LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "first-namespace"},
		Spec:       LogicalContainerSpec{Mode: LogicalContainerModePersistent, ContainerName: "runtime-container"},
	}
	second := first.DeepCopy()
	second.Name, second.Namespace = "second", "second-namespace"
	require.Equal(t, "containers/runtime-container", first.GetLeaseKey())
	require.Equal(t, first.GetLeaseKey(), second.GetLeaseKey())
}

// Verifies that LogicalContainer JSON separates lifecycle policy from the container,
// uses networkRef for network references, and retains observed identity and lifecycle status.
func TestLogicalContainerJSONShape(t *testing.T) {
	exitCode := int32(0)
	container := &LogicalContainer{
		TypeMeta:   metav1.TypeMeta{Kind: "LogicalContainer", APIVersion: GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "test-namespace"},
		Spec: LogicalContainerSpec{
			ContainerName: "runtime-container", Mode: LogicalContainerModePersistent, LifecycleKey: "desired-key",
			Container: &LogicalContainerConfig{
				Image:    &PhysicalContainerImageConfig{Image: "example/image:latest"},
				Networks: []LogicalContainerNetworkConnectionConfig{{NetworkRef: "network"}},
			},
		},
		Status: LogicalContainerStatus{
			Phase: LogicalContainerPhaseRunning, PhysicalContainerRef: "physical-container", ImageRef: "physical-image",
			ContainerID: "runtime-id", ContainerName: "runtime-container", LifecycleKey: "desired-key",
			ExitCode: &exitCode,
			Conditions: []metav1.Condition{{
				Type: string(ConditionReady), Status: metav1.ConditionTrue, Reason: "ContainerRunning",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	data, marshalErr := json.Marshal(container)
	require.NoError(t, marshalErr)
	var fields struct {
		Spec   map[string]json.RawMessage `json:"spec"`
		Status map[string]json.RawMessage `json:"status"`
	}
	require.NoError(t, json.Unmarshal(data, &fields))
	require.JSONEq(t, `{"image":{"image":"example/image:latest"},"networks":[{"networkRef":"network"}]}`, string(fields.Spec["container"]))
	require.JSONEq(t, `"persistent"`, string(fields.Spec["mode"]))
	require.JSONEq(t, `"runtime-container"`, string(fields.Spec["containerName"]))
	require.NotContains(t, fields.Spec, "containerID")
	require.NotContains(t, fields.Spec, "persistent")
	require.NotContains(t, fields.Status, "message")
	require.JSONEq(t, `"runtime-id"`, string(fields.Status["containerID"]))
	require.JSONEq(t, `"desired-key"`, string(fields.Status["lifecycleKey"]))
	require.JSONEq(t, `0`, string(fields.Status["exitCode"]))

	var restored LogicalContainer
	require.NoError(t, json.Unmarshal(data, &restored))
	require.Equal(t, container.Spec, restored.Spec)
	require.Equal(t, container.Status.Phase, restored.Status.Phase)
	require.Equal(t, container.Status.Conditions[0].Reason, restored.Status.Conditions[0].Reason)
}

// Verifies that LogicalContainer.DeepCopy and LogicalContainerStatus.CopyTo isolate nested container and observed status data.
func TestLogicalContainerDeepCopy(t *testing.T) {
	exitCode := int32(0)
	container := &LogicalContainer{
		Spec: LogicalContainerSpec{Container: &LogicalContainerConfig{
			Image:    &PhysicalContainerImageConfig{Build: &ContainerBuildContext{Context: "/source", Labels: []commonapi.Label{{Key: "key", Value: "value"}}}},
			Command:  []string{"command"},
			Networks: []LogicalContainerNetworkConnectionConfig{{NetworkRef: "network", Aliases: []string{"alias"}}},
			CreateFiles: []CreateFileSystem{{
				Destination: "/app", Entries: []FileSystemEntry{{Name: "directory", Type: FileSystemEntryTypeDir, Entries: []FileSystemEntry{{Name: "file", Contents: "value"}}}},
			}},
		}},
		Status: LogicalContainerStatus{
			ExitCode:     &exitCode,
			PortMappings: []PhysicalContainerPortMapping{{ContainerPort: 8080}},
			Conditions:   []metav1.Condition{{Type: string(ConditionReady), Message: "ready"}},
		},
	}
	copied := container.DeepCopy()
	require.Equal(t, container, copied)
	copied.Spec.Container.Image.Build.Labels[0].Value = "changed"
	copied.Spec.Container.Command[0] = "changed"
	copied.Spec.Container.Networks[0].Aliases[0] = "changed"
	copied.Spec.Container.CreateFiles[0].Entries[0].Entries[0].Contents = "changed"
	require.Equal(t, "value", container.Spec.Container.Image.Build.Labels[0].Value)
	require.Equal(t, "command", container.Spec.Container.Command[0])
	require.Equal(t, "alias", container.Spec.Container.Networks[0].Aliases[0])
	require.Equal(t, "value", container.Spec.Container.CreateFiles[0].Entries[0].Entries[0].Contents)

	destination := &LogicalContainer{}
	container.GetStatus().CopyTo(destination)
	*destination.Status.ExitCode = 1
	destination.Status.PortMappings[0].ContainerPort = 9090
	destination.Status.Conditions[0].Message = "changed"
	require.Equal(t, int32(0), *container.Status.ExitCode)
	require.Equal(t, int32(8080), container.Status.PortMappings[0].ContainerPort)
	require.Equal(t, "ready", container.Status.Conditions[0].Message)
}
