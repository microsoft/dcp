package containers

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
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

func AddFileToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, mode fs.FileMode, file apiv1.FileSystemEntry, modTime time.Time) error {
	if file.Type != "" && file.Type != apiv1.FileSystemEntryTypeFile {
		return fmt.Errorf("item is not a file")
	}

	if file.Mode != 0 {
		mode = file.Mode
	}

	if file.Owner != nil {
		owner = *file.Owner
	}

	if file.Group != nil {
		group = *file.Group
	}

	basePath = path.Join(basePath, file.Name)

	return tarWriter.WriteFile([]byte(file.Contents), basePath, owner, group, mode, modTime, modTime, modTime)
}

func AddDirectoryToTar(tarWriter *usvc_io.TarWriter, basePath string, owner int32, group int32, mode fs.FileMode, directory apiv1.FileSystemEntry, modTime time.Time) error {
	if directory.Type != apiv1.FileSystemEntryTypeDir {
		return fmt.Errorf("item is not a directory")
	}

	if directory.Mode != 0 {
		mode = directory.Mode
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
			if addDirectoryErr := AddDirectoryToTar(tarWriter, basePath, owner, group, mode, item, modTime); addDirectoryErr != nil {
				return addDirectoryErr
			}
		} else {
			if addFileErr := AddFileToTar(tarWriter, basePath, owner, group, mode, item, modTime); addFileErr != nil {
				return addFileErr
			}
		}
	}

	return nil
}
