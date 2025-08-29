package containers

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/openssl"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

const (
	ContainerLogsHttpPath string = "/apis/usvc-dev.developer.microsoft.com/v1/containers/%s/log"
)

var (
	ErrUnmatched         = errors.New("error")
	ErrUnmarshalling     = errors.New("error unmarshalling object")
	ErrNotFound          = errors.New("object not found")
	ErrAlreadyExists     = errors.New("object already exists")
	ErrCouldNotAllocate  = errors.New("object could not allocate required resources")
	ErrRuntimeNotHealthy = errors.New("runtime is not healthy")
	ErrObjectInUse       = errors.New("object is in use")
	ErrIncomplete        = errors.New("not all requested objects were returned")
	DanglingFilterTrue   = true
	DanglingFilterFalse  = false
)

func (o StreamContainerLogsOptions) Apply(args []string) []string {
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Timestamps {
		args = append(args, "--timestamps")
	}
	return args
}

func AddCertificateToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, umask fs.FileMode, certificate apiv1.FileSystemEntry, modTime time.Time, hashes []string, log logr.Logger) (string, error) {
	if certificate.Type != apiv1.FileSystemEntryTypeOpenSSL {
		return "", fmt.Errorf("item is not a certificate")
	}

	mode := certificate.Mode
	if mode == 0 {
		mode = osutil.DefaultFileBitmask &^ umask
	}

	if certificate.Owner != nil {
		owner = *certificate.Owner
	}

	if certificate.Group != nil {
		group = *certificate.Group
	}

	certPath := path.Join(basePath, certificate.Name)

	var contents []byte
	if certificate.Source != "" {
		stat, statErr := os.Stat(certificate.Source)
		if statErr != nil {
			return "", fmt.Errorf("could not stat file %s: %w", certificate.Source, statErr)
		}

		if stat.Size() > osutil.MaxCopyFileSize {
			return "", fmt.Errorf("file %s exceeds max supported file size (%d bytes): %d bytes", certificate.Source, osutil.MaxCopyFileSize, stat.Size())
		}

		f, openErr := usvc_io.OpenFile(certificate.Source, os.O_RDONLY, 0)
		if openErr != nil {
			return "", fmt.Errorf("could not open %s: %w", certificate.Source, openErr)
		}
		defer f.Close()

		var readErr error
		contents, readErr = io.ReadAll(f)
		if readErr != nil {
			return "", fmt.Errorf("could not read contents of %s: %w", certificate.Source, readErr)
		}
	} else {
		contents = []byte(certificate.Contents)
	}

	log.V(1).Info("Writing certificate to tar", "Certificate", certPath)

	writeErr := tarWriter.WriteFile(contents, certPath, owner, group, mode, modTime, modTime, modTime)
	if writeErr != nil {
		return "", writeErr
	}

	block, _ := pem.Decode(contents)
	if block == nil {
		return "", fmt.Errorf("could not decode PEM block from certificate %s", certPath)
	} else if block.Type != "CERTIFICATE" {
		// Expected a PEM encoded public certificate, but received something else
		return "", fmt.Errorf("expected a single PEM format public certificate for %s, but received something else: %s", certPath, block.Type)
	}

	certBytes := block.Bytes

	x509Cert, certErr := x509.ParseCertificate(certBytes)
	if certErr != nil {
		return "", fmt.Errorf("could not parse certificate %s: %w", certPath, certErr)
	}

	shortHash, hashErr := openssl.SubjectHash(x509Cert)
	if hashErr != nil {
		return "", fmt.Errorf("could not compute subject hash for certificate %s: %w", certPath, hashErr)
	}

	hashCollisions := 0
	for _, existingHash := range hashes {
		if existingHash == shortHash {
			hashCollisions++
		}
	}

	log.V(1).Info("Writing subject hash symlink", "Certificate", certPath, "Hash", shortHash, "Collisions", hashCollisions)

	return shortHash, tarWriter.WriteSymlink(path.Join(basePath, fmt.Sprintf("%s.%d", shortHash, hashCollisions)), "./"+certificate.Name, owner, group, modTime, modTime, modTime)
}

func AddFileToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, umask fs.FileMode, file apiv1.FileSystemEntry, modTime time.Time, log logr.Logger) error {
	if file.Type != "" && file.Type != apiv1.FileSystemEntryTypeFile {
		return fmt.Errorf("item is not a file")
	}

	mode := file.Mode
	if mode == 0 {
		mode = osutil.DefaultFileBitmask &^ umask
	}

	if file.Owner != nil {
		owner = *file.Owner
	}

	if file.Group != nil {
		group = *file.Group
	}

	basePath = path.Join(basePath, file.Name)

	if file.Source != "" {
		stat, statErr := os.Stat(file.Source)
		if statErr != nil {
			return fmt.Errorf("could not stat file %s: %w", file.Source, statErr)
		}

		if stat.Size() > osutil.MaxCopyFileSize {
			return fmt.Errorf("file %s exceeds max supported file size (%d bytes): %d bytes", file.Source, osutil.MaxCopyFileSize, stat.Size())
		}

		log.V(1).Info("copying file to tar", "file", file.Source, "size", stat.Size())

		f, openErr := usvc_io.OpenFile(file.Source, os.O_RDONLY, 0)
		if openErr != nil {
			return fmt.Errorf("could not open file %s: %w", file.Source, openErr)
		}
		defer f.Close()
		return tarWriter.CopyFile(f, stat.Size(), basePath, owner, group, mode, stat.ModTime(), stat.ModTime(), stat.ModTime())
	} else {
		return tarWriter.WriteFile([]byte(file.Contents), basePath, owner, group, mode, modTime, modTime, modTime)
	}
}

func AddSymlinkToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, umask fs.FileMode, symlink apiv1.FileSystemEntry, modTime time.Time, log logr.Logger) error {
	if symlink.Type != apiv1.FileSystemEntryTypeSymlink {
		return fmt.Errorf("item is not a symlink")
	}

	if symlink.Owner != nil {
		owner = *symlink.Owner
	}

	if symlink.Group != nil {
		group = *symlink.Group
	}

	basePath = path.Join(basePath, symlink.Name)

	return tarWriter.WriteSymlink(basePath, symlink.Target, owner, group, modTime, modTime, modTime)
}

func AddDirectoryToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, umask fs.FileMode, directory apiv1.FileSystemEntry, modTime time.Time, log logr.Logger) error {
	if directory.Type != apiv1.FileSystemEntryTypeDir {
		return fmt.Errorf("item is not a directory")
	}

	mode := directory.Mode
	if mode == 0 {
		mode = osutil.DefaultFolderBitmask &^ umask
	}

	if directory.Owner != nil {
		owner = *directory.Owner
	}

	if directory.Group != nil {
		group = *directory.Group
	}

	basePath = path.Join(basePath, directory.Name)

	err := tarWriter.WriteDir(basePath, owner, group, mode, modTime, modTime, modTime)

	if err != nil {
		return err
	}

	certificateHashes := []string{}

	for _, item := range directory.Entries {
		switch item.Type {
		case apiv1.FileSystemEntryTypeDir:
			if addDirectoryErr := AddDirectoryToTar(tarWriter, basePath, owner, group, umask, item, modTime, log); addDirectoryErr != nil {
				return addDirectoryErr
			}
		case apiv1.FileSystemEntryTypeSymlink:
			if addSymlinkErr := AddSymlinkToTar(tarWriter, basePath, owner, group, umask, item, modTime, log); addSymlinkErr != nil {
				return addSymlinkErr
			}
		case apiv1.FileSystemEntryTypeOpenSSL:
			hash, addCertErr := AddCertificateToTar(tarWriter, basePath, owner, group, umask, item, modTime, certificateHashes, log)
			if addCertErr != nil {
				return addCertErr
			}

			// Keep track of the certificate hashes we've added to this directory so that we can deal with the possibility of collisions
			certificateHashes = append(certificateHashes, hash)
		default:
			// Catchall for specific file types (generic files or certificates)
			if addFileErr := AddFileToTar(tarWriter, basePath, owner, group, umask, item, modTime, log); addFileErr != nil {
				return addFileErr
			}
		}
	}

	return nil
}
