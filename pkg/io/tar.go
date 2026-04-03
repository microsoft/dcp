/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

type TarWriter struct {
	writer *tar.Writer
	buffer *bytes.Buffer
}

// NewTarWriter creates a TarWriter that buffers the tar archive in memory.
// Use Buffer() to retrieve the completed archive.
func NewTarWriter() *TarWriter {
	buffer := &bytes.Buffer{}
	return &TarWriter{
		writer: tar.NewWriter(buffer),
		buffer: buffer,
	}
}

// NewTarWriterTo creates a TarWriter that writes directly to the provided writer.
// Use Close() to finalize the tar archive when done writing entries.
// Buffer() is not supported on writers created with NewTarWriterTo.
func NewTarWriterTo(w io.Writer) *TarWriter {
	return &TarWriter{
		writer: tar.NewWriter(w),
	}
}

func (tw *TarWriter) Buffer() (*bytes.Buffer, error) {
	if tw.buffer == nil {
		return nil, fmt.Errorf("Buffer() is not supported on TarWriters created with NewTarWriterTo; use Close() instead")
	}

	err := tw.writer.Close()
	if err != nil {
		return nil, err
	}
	return tw.buffer, nil
}

// Close finalizes the tar archive. Use this instead of Buffer() when
// the TarWriter was created with NewTarWriterTo.
func (tw *TarWriter) Close() error {
	return tw.writer.Close()
}

func (tw *TarWriter) WriteDir(name string, uid int32, gid int32, mode os.FileMode, modTime time.Time, changeTime time.Time, accessTime time.Time) error {
	header := &tar.Header{
		Name:       name,
		Uid:        int(uid),
		Gid:        int(gid),
		Mode:       int64(mode | fs.ModeDir),
		ModTime:    modTime,
		ChangeTime: changeTime,
		AccessTime: accessTime,
		Typeflag:   tar.TypeDir,
	}

	err := tw.writer.WriteHeader(header)
	if err != nil {
		return err
	}

	return nil
}

func (tw *TarWriter) WriteSymlink(name string, linkTarget string, uid int32, gid int32, modTime time.Time, changeTime time.Time, accessTime time.Time) error {
	header := &tar.Header{
		Name:       name,
		Linkname:   linkTarget,
		Uid:        int(uid),
		Gid:        int(gid),
		ModTime:    modTime,
		ChangeTime: changeTime,
		AccessTime: accessTime,
		Typeflag:   tar.TypeSymlink,
	}

	err := tw.writer.WriteHeader(header)
	if err != nil {
		return err
	}

	return nil
}

func (tw *TarWriter) WriteFile(contents []byte, name string, uid int32, gid int32, mode os.FileMode, modTime time.Time, changeTime time.Time, accessTime time.Time) error {
	header := &tar.Header{
		Name:       name,
		Size:       int64(len(contents)),
		Uid:        int(uid),
		Gid:        int(gid),
		Mode:       int64(mode),
		ModTime:    modTime,
		ChangeTime: changeTime,
		AccessTime: accessTime,
		Typeflag:   tar.TypeReg,
	}

	err := tw.writer.WriteHeader(header)
	if err != nil {
		return err
	}

	n, writeErr := tw.writer.Write(contents)
	if writeErr != nil {
		return writeErr
	}

	if n < len(contents) {
		return io.ErrShortWrite
	}

	return nil
}

func (tw *TarWriter) CopyFile(src io.Reader, size int64, name string, uid int32, gid int32, mode os.FileMode, modTime time.Time, changeTime time.Time, accessTime time.Time) error {
	header := &tar.Header{
		Name:       name,
		Size:       size,
		Uid:        int(uid),
		Gid:        int(gid),
		Mode:       int64(mode),
		ModTime:    modTime,
		ChangeTime: changeTime,
		AccessTime: accessTime,
		Typeflag:   tar.TypeReg,
	}

	err := tw.writer.WriteHeader(header)
	if err != nil {
		return err
	}

	n, writeErr := io.Copy(tw.writer, src)
	if writeErr != nil {
		return writeErr
	}

	if n < size {
		return io.ErrShortWrite
	}

	return nil
}
