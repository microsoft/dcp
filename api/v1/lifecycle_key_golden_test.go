/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenContainerLifecycleKey is the lifecycle key produced by goldenContainerSpec.
// Lifecycle keys identify already-running containers across DCP restarts, so a change
// to this value silently orphans existing containers and recreates them.
//
// The key is derived from gob encodings of the types registered in
// initializeLifecycleHashEncoder. It changes if any of the following change:
// an encoded type's name, an encoded type's exported field list, or the order of
// registrations in initializeLifecycleHashEncoder (gob assigns type IDs from a
// process-global counter in registration order). Note that the package a type lives
// in is not part of its gob identity.
//
// If this test fails, do not update the constant to match. Restore the encoding instead,
// or accept that every running container will be recreated on upgrade.
const goldenContainerLifecycleKey = "2e098af5d07606845e7c9237c0f8b826"

// goldenContainerSpec exercises every field group that contributes to the lifecycle key.
func goldenContainerSpec() *ContainerSpec {
	umask := fs.FileMode(0o022)
	owner := int32(1000)

	return &ContainerSpec{
		Image:         "api:dev",
		ContainerName: "api",
		Persistent:    true,
		Build: &ContainerBuildContext{
			Context:    "/src",
			Dockerfile: "/src/Dockerfile.golden",
			Stage:      "final",
			Platform:   "linux/amd64",
			Labels: []ContainerLabel{
				{Key: "com.example.b", Value: "two"},
				{Key: "com.example.a", Value: "one"},
			},
			Secrets: []ContainerBuildSecret{
				{Type: EnvSecret, ID: "npm_token", Source: "NPM_TOKEN"},
				{Type: FileSecret, ID: "aws", Source: "/nonexistent/aws.json"},
			},
		},
		VolumeMounts: []VolumeMount{
			{Type: BindMount, Source: "/host/data", Target: "/data", ReadOnly: true},
			{Type: NamedVolumeMount, Source: "cache", Target: "/cache"},
		},
		Ports: []ContainerPort{
			{HostPort: 18080, ContainerPort: 8080, Protocol: TCP, HostIP: "127.0.0.1"},
			{HostPort: 15353, ContainerPort: 53, Protocol: UDP},
		},
		Env: []EnvVar{
			{Name: "B_VAR", Value: "2"},
			{Name: "A_VAR", Value: "1"},
		},
		CreateFiles: []CreateFileSystem{
			{
				Destination:  "/etc/golden",
				DefaultOwner: 1000,
				DefaultGroup: 1000,
				Umask:        &umask,
				Entries: []FileSystemEntry{
					{
						Type:  FileSystemEntryTypeDir,
						Name:  "conf.d",
						Owner: &owner,
						Mode:  0o755,
						Entries: []FileSystemEntry{
							{Type: FileSystemEntryTypeFile, Name: "app.conf", Contents: "x=1", Mode: 0o644},
						},
					},
				},
			},
		},
		ImageLayers: []ImageLayer{
			{Digest: "sha256:golden-layer-1", RawContents: "AAAA"},
		},
		PemCertificates: &ContainerPemCertificates{
			Certificates:         []PemCertificate{{Thumbprint: "AABB", Contents: "-----BEGIN CERTIFICATE-----"}},
			Destination:          "/certs",
			OverwriteBundlePaths: []string{"/etc/ssl/certs/ca-certificates.crt"},
		},
		Terminal: &TerminalSpec{UDSPath: "/tmp/api.sock", Cols: 80, Rows: 24},
	}
}

func TestContainerSpecLifecycleKeyIsStable(t *testing.T) {
	t.Parallel()

	key, computed, err := goldenContainerSpec().GetLifecycleKey()
	require.NoError(t, err)
	require.True(t, computed)
	require.Equal(
		t,
		goldenContainerLifecycleKey,
		key,
		"container lifecycle key changed; existing containers would be orphaned and recreated",
	)
}
