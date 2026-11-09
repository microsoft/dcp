/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import "os"

// AppendFile exposes only sequential append operations for a file opened in append mode.
type AppendFile struct {
	file *os.File
}

func newAppendFile(file *os.File) *AppendFile {
	return &AppendFile{file: file}
}

// Name returns the file name supplied when the file was opened.
func (file *AppendFile) Name() string {
	return file.file.Name()
}

// Write appends data to the file.
func (file *AppendFile) Write(data []byte) (int, error) {
	return file.file.Write(data)
}

// WriteString appends a string to the file.
func (file *AppendFile) WriteString(data string) (int, error) {
	return file.file.WriteString(data)
}

// Sync commits the current contents of the file to stable storage.
func (file *AppendFile) Sync() error {
	return file.file.Sync()
}

// Close closes the file.
func (file *AppendFile) Close() error {
	return file.file.Close()
}

var _ WriteSyncerCloser = (*AppendFile)(nil)
