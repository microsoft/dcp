/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/osutil"
)

// readTarEntries reads a tar archive and returns a map of entry name to its string contents.
func readTarEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	entries := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		var contents bytes.Buffer
		if header.Typeflag == tar.TypeReg {
			_, copyErr := io.Copy(&contents, reader)
			require.NoError(t, copyErr)
		} else if header.Typeflag == tar.TypeSymlink {
			contents.WriteString(header.Linkname)
		}
		entries[header.Name] = contents.String()
	}
	return entries
}

func TestBuildCreateFilesTar_FilesAndSymlink(t *testing.T) {
	options := CreateFilesOptions{
		Container:   "ignored",
		Destination: "/etc/app",
		Umask:       osutil.DefaultUmaskBitmask,
		Entries: []apiv1.FileSystemEntry{
			{
				Name:     "config.txt",
				Contents: "hello",
			},
			{
				Name:   "link",
				Type:   apiv1.FileSystemEntryTypeSymlink,
				Target: "./config.txt",
			},
		},
	}

	tarBytes, err := BuildCreateFilesTar(options, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, tarBytes)

	entries := readTarEntries(t, tarBytes)
	assert.Equal(t, "hello", entries["/etc/app/config.txt"])
	assert.Equal(t, "./config.txt", entries["/etc/app/link"])
}

func TestBuildCreateFilesTar_EmptyReturnsNil(t *testing.T) {
	options := CreateFilesOptions{
		Container:   "ignored",
		Destination: "/etc/app",
		Umask:       osutil.DefaultUmaskBitmask,
		Entries:     []apiv1.FileSystemEntry{},
	}

	tarBytes, err := BuildCreateFilesTar(options, logr.Discard())
	require.NoError(t, err)
	assert.Nil(t, tarBytes)
}

func TestBuildCreateFilesTar_Directory(t *testing.T) {
	options := CreateFilesOptions{
		Container:   "ignored",
		Destination: "/etc/app",
		Umask:       osutil.DefaultUmaskBitmask,
		Entries: []apiv1.FileSystemEntry{
			{
				Name: "certs",
				Type: apiv1.FileSystemEntryTypeDir,
				Entries: []apiv1.FileSystemEntry{
					{
						Name:     "bundle.pem",
						Contents: "cert-bytes",
					},
				},
			},
		},
	}

	tarBytes, err := BuildCreateFilesTar(options, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, tarBytes)

	entries := readTarEntries(t, tarBytes)
	assert.Equal(t, "cert-bytes", entries["/etc/app/certs/bundle.pem"])
}
