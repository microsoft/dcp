/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func TestCreateFilesArchiveMatchesExistingAssembly(t *testing.T) {
	t.Parallel()

	certificate := createArchiveTestCertificate(t)
	fileOwner := int32(1001)
	fileGroup := int32(1002)
	modTime := time.Date(2026, time.September, 13, 20, 15, 0, 0, time.UTC)
	options := CreateFilesOptions{
		ModTime:      modTime,
		Destination:  "/workspace",
		DefaultOwner: 101,
		DefaultGroup: 202,
		Umask:        0027,
		Entries: []FileSystemEntry{
			{
				Name:     "inline.txt",
				Contents: "inline contents",
				Owner:    &fileOwner,
				Group:    &fileGroup,
				Mode:     0640,
			},
			{
				Name:        "raw.bin",
				RawContents: base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 3}),
			},
			{
				Type:   FileSystemEntryTypeSymlink,
				Name:   "inline-link",
				Target: "./inline.txt",
			},
			{
				Type: FileSystemEntryTypeDir,
				Name: "nested",
				Mode: 0750,
				Entries: []FileSystemEntry{
					{Name: "nested.txt", Contents: "nested contents"},
					{Type: FileSystemEntryTypeSymlink, Name: "nested-link", Target: "./nested.txt"},
				},
			},
			{
				Type:     FileSystemEntryTypeOpenSSL,
				Name:     "certificate-a.pem",
				Contents: certificate,
			},
			{
				Type:     FileSystemEntryTypeOpenSSL,
				Name:     "certificate-b.pem",
				Contents: certificate,
			},
		},
	}

	expected := assembleCreateFilesArchiveDirectly(t, options)
	actual, archiveErr := CreateFilesArchive(context.Background(), logr.Discard(), options)

	require.NoError(t, archiveErr)
	require.NotNil(t, actual)
	assert.Equal(t, expected.Bytes(), actual.Bytes())

	var certificateLinks []string
	reader := tar.NewReader(bytes.NewReader(actual.Bytes()))
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		require.NoError(t, nextErr)
		if header.Typeflag == tar.TypeSymlink && strings.HasPrefix(header.Linkname, "./certificate-") {
			certificateLinks = append(certificateLinks, header.Name)
		}
	}
	require.Len(t, certificateLinks, 2)
	assert.True(t, strings.HasSuffix(certificateLinks[0], ".0"))
	assert.True(t, strings.HasSuffix(certificateLinks[1], ".1"))
}

func TestCreateFilesArchiveReturnsNilWhenAllIgnorableEntriesFail(t *testing.T) {
	t.Parallel()

	buffer, archiveErr := CreateFilesArchive(context.Background(), logr.Discard(), CreateFilesOptions{
		Destination: "/workspace",
		Entries: []FileSystemEntry{
			{Name: "bad-raw", RawContents: "%%%", ContinueOnError: true},
			{Type: FileSystemEntryTypeOpenSSL, Name: "bad-cert.pem", Contents: "not a certificate", ContinueOnError: true},
		},
	})

	require.NoError(t, archiveErr)
	assert.Nil(t, buffer)
}

func TestCreateFilesArchiveReturnsEntryError(t *testing.T) {
	t.Parallel()

	buffer, archiveErr := CreateFilesArchive(context.Background(), logr.Discard(), CreateFilesOptions{
		Destination: "/workspace",
		Entries: []FileSystemEntry{
			{Name: "bad-raw", RawContents: "%%%"},
		},
	})

	require.Error(t, archiveErr)
	assert.Contains(t, archiveErr.Error(), "could not decode rawContents")
	assert.Nil(t, buffer)
}

func TestCreateFilesArchiveHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	buffer, archiveErr := CreateFilesArchive(ctx, logr.Discard(), CreateFilesOptions{
		Destination: "/workspace",
		Entries: []FileSystemEntry{
			{Name: "file.txt", Contents: "contents"},
		},
	})

	require.ErrorIs(t, archiveErr, context.Canceled)
	assert.Nil(t, buffer)
}

func assembleCreateFilesArchiveDirectly(t *testing.T, options CreateFilesOptions) *bytes.Buffer {
	t.Helper()

	tarWriter := usvc_io.NewTarWriter()
	certificateHashes := []string{}
	for _, item := range options.Entries {
		switch item.Type {
		case FileSystemEntryTypeDir:
			require.NoError(t, AddDirectoryToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, logr.Discard()))
		case FileSystemEntryTypeSymlink:
			require.NoError(t, AddSymlinkToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, logr.Discard()))
		case FileSystemEntryTypeOpenSSL:
			hash, addCertificateErr := AddCertificateToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, certificateHashes, logr.Discard())
			require.NoError(t, addCertificateErr)
			certificateHashes = append(certificateHashes, hash)
		default:
			require.NoError(t, AddFileToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, logr.Discard()))
		}
	}

	buffer, bufferErr := tarWriter.Buffer()
	require.NoError(t, bufferErr)
	return buffer
}

func createArchiveTestCertificate(t *testing.T) string {
	t.Helper()

	publicKey, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, keyErr)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "CreateFilesArchive test"},
		NotBefore:             time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	derBytes, createErr := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	require.NoError(t, createErr)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
}
