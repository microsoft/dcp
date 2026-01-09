// Copyright (c) Microsoft Corporation. All rights reserved.

package io_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

func TestMultipleRwOps(t *testing.T) {
	reader, writer := usvc_io.NewBufferedPipe()
	sync := make(chan struct{})

	doReading := func(what string) {
		buf := make([]byte, 100)
		n, err := reader.Read(buf)
		require.NoError(t, err)
		require.Equal(t, len(what), n)
		require.Equal(t, what, string(buf[0:n]))
		sync <- struct{}{}
	}

	doWriting := func(what string) {
		n, err := writer.Write([]byte(what))
		require.NoError(t, err)
		require.Equal(t, len(what), n)
	}

	doWriting("alpha")
	go doReading("alpha")
	<-sync

	doWriting("bravo")
	go doReading("bravo")
	<-sync
}
