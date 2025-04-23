package containers

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

const (
	ContainerLogsHttpPath string = "/apis/usvc-dev.developer.microsoft.com/v1/containers/%s/log"
)

var (
	ErrNotFound          = errors.New("object not found")
	ErrAlreadyExists     = errors.New("object already exists")
	ErrCouldNotAllocate  = errors.New("object could not allocate required resources")
	ErrRuntimeNotHealthy = errors.New("runtime is not healthy")
	ErrObjectInUse       = errors.New("object is in use")
)

func (o StreamContainerLogsOptions) Apply(args []string) []string {
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Timestamps {
		args = append(args, "--timestamps")
	}
	if o.Tail != 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", o.Tail))
	}
	if !o.Since.IsZero() {
		args = append(args, "--since", o.Since.Format(time.RFC3339))
	}
	if !o.Until.IsZero() {
		args = append(args, "--until", o.Until.Format(time.RFC3339))
	}
	return args
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

	for _, item := range directory.Entries {
		if item.Type == apiv1.FileSystemEntryTypeDir {
			if addDirectoryErr := AddDirectoryToTar(tarWriter, basePath, owner, group, umask, item, modTime, log); addDirectoryErr != nil {
				return addDirectoryErr
			}
		} else {
			if addFileErr := AddFileToTar(tarWriter, basePath, owner, group, umask, item, modTime, log); addFileErr != nil {
				return addFileErr
			}
		}
	}

	return nil
}
