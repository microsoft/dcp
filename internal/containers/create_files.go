/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"context"

	"github.com/go-logr/logr"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// CreateFilesArchive creates the tar archive consumed by container copy commands.
// A nil buffer and nil error indicate that no entries were written.
func CreateFilesArchive(ctx context.Context, log logr.Logger, options CreateFilesOptions) (*bytes.Buffer, error) {
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return nil, cancellationErr
	}

	tarWriter := usvc_io.NewTarWriter()
	certificateHashes := []string{}

	for _, item := range options.Entries {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return nil, cancellationErr
		}

		switch item.Type {
		case FileSystemEntryTypeDir:
			if addDirectoryErr := AddDirectoryToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, log); addDirectoryErr != nil {
				return nil, addDirectoryErr
			}
		case FileSystemEntryTypeSymlink:
			if addSymlinkErr := AddSymlinkToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, log); addSymlinkErr != nil {
				if item.ContinueOnError {
					log.Error(addSymlinkErr, "Failed to add symlink to tar archive, continuing", "SymLink", item)
				} else {
					return nil, addSymlinkErr
				}
			}
		case FileSystemEntryTypeOpenSSL:
			hash, addCertificateErr := AddCertificateToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, certificateHashes, log)
			if addCertificateErr != nil {
				if item.ContinueOnError {
					log.Error(addCertificateErr, "Failed to add a certificate to the tar file, but continueOnError is set", "Certificate", item)
				} else {
					return nil, addCertificateErr
				}
			}

			certificateHashes = append(certificateHashes, hash)
		default:
			if addFileErr := AddFileToTar(tarWriter, options.Destination, options.DefaultOwner, options.DefaultGroup, options.Umask, item, options.ModTime, log); addFileErr != nil {
				if item.ContinueOnError {
					log.Error(addFileErr, "Failed to add a file to the tar file, but continueOnError is set", "File", item)
				} else {
					return nil, addFileErr
				}
			}
		}
	}

	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return nil, cancellationErr
	}
	if tarWriter.Empty() {
		return nil, nil
	}

	buffer, bufferErr := tarWriter.Buffer()
	if bufferErr != nil {
		return nil, bufferErr
	}
	return buffer, nil
}
