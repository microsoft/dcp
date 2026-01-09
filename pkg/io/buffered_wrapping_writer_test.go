/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func TestTargetWriterSetImmediately(t *testing.T) {
	t.Parallel()

	bww := usvc_io.NewBufferedWrappingWriter()
	target := &bytes.Buffer{}
	err := bww.SetTarget(target)
	require.NoError(t, err)

	content := "alpha"
	n, err := bww.Write([]byte(content))
	require.NoError(t, err)
	require.Equal(t, len(content), n)
}

func TestTargetWriterSetAfterWrite(t *testing.T) {
	t.Parallel()

	bww := usvc_io.NewBufferedWrappingWriter()

	content := "alpha"
	n, err := bww.Write([]byte(content))
	require.NoError(t, err)
	require.Equal(t, len(content), n)

	target := &bytes.Buffer{}
	err = bww.SetTarget(target)
	require.NoError(t, err)
	require.Equal(t, content, target.String())
}

func TestWriterUsableAfterTargetSet(t *testing.T) {
	t.Parallel()

	bww := usvc_io.NewBufferedWrappingWriter()

	content1 := "alpha"
	n, err := bww.Write([]byte(content1))
	require.NoError(t, err)
	require.Equal(t, len(content1), n)

	target := &bytes.Buffer{}
	err = bww.SetTarget(target)
	require.NoError(t, err)

	content2 := "foxtrot"
	n, err = bww.Write([]byte(content2))
	require.NoError(t, err)
	require.Equal(t, len(content2), n)

	require.Equal(t, content1+content2, target.String())
}
