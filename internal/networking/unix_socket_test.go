/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/pkg/osutil"
)

// shortTempDir creates a short temporary directory for socket tests.
// macOS has a ~104 character limit for Unix socket paths, so we use
// a short base path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, dirErr := os.MkdirTemp("", "sck")
	require.NoError(t, dirErr)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestPrepareSecureSocketDirCreatesDirectoryWithCorrectPermissions(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	socketDir, prepareErr := PreparePrivateUnixSocketDir(rootDir)
	require.NoError(t, prepareErr)

	expectedDir := filepath.Join(rootDir, dcppaths.DcpWorkDir)
	assert.Equal(t, expectedDir, socketDir)

	info, statErr := os.Stat(socketDir)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
	if runtime.GOOS != "windows" {
		assert.Equal(t, osutil.PermissionOnlyOwnerReadWriteTraverse, info.Mode().Perm())
	}
}

func TestPrepareSecureSocketDirIdempotentOnRepeatedCalls(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	dir1, err1 := PreparePrivateUnixSocketDir(rootDir)
	require.NoError(t, err1)

	dir2, err2 := PreparePrivateUnixSocketDir(rootDir)
	require.NoError(t, err2)

	assert.Equal(t, dir1, dir2)
}

func TestPrepareSecureSocketDirFallsBackToUserCacheDir(t *testing.T) {
	t.Parallel()

	socketDir, prepareErr := PreparePrivateUnixSocketDir("")
	require.NoError(t, prepareErr)

	cacheDir, cacheDirErr := os.UserCacheDir()
	require.NoError(t, cacheDirErr)

	expectedDir := filepath.Join(cacheDir, dcppaths.DcpWorkDir)
	assert.Equal(t, expectedDir, socketDir)
}

func TestPrepareSecureSocketDirRejectsWrongPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission validation is skipped on Windows")
	}

	t.Parallel()
	rootDir := shortTempDir(t)

	// Pre-create the dcp-work directory with overly-permissive permissions
	socketDir := filepath.Join(rootDir, dcppaths.DcpWorkDir)
	mkdirErr := os.MkdirAll(socketDir, 0755)
	require.NoError(t, mkdirErr)

	_, prepareErr := PreparePrivateUnixSocketDir(rootDir)
	require.Error(t, prepareErr)
	assert.Contains(t, prepareErr.Error(), "not private to the user")
}

func TestPrivateUnixSocketListenerCreatesListenerWithRandomName(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "test-")
	require.NoError(t, createErr)
	require.NotNil(t, listener)
	defer listener.Close()

	socketPath := listener.SocketPath()
	socketName := filepath.Base(socketPath)

	// Verify the socket name starts with the prefix and has the random suffix
	assert.True(t, len(socketName) > len("test-"), "socket name should include random suffix")
	assert.Equal(t, "test-", socketName[:len("test-")])

	// Verify socket file was created
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr)
}

func TestPrivateUnixSocketListenerAcceptsConnections(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "acc-")
	require.NoError(t, createErr)
	defer listener.Close()

	// Accept in background
	var serverConn net.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, acceptErr = listener.Accept()
	}()

	// Connect client
	clientConn, dialErr := net.Dial("unix", listener.SocketPath())
	require.NoError(t, dialErr)
	defer clientConn.Close()

	wg.Wait()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)
	defer serverConn.Close()

	// Verify we can exchange data
	_, writeErr := clientConn.Write([]byte("hello"))
	require.NoError(t, writeErr)

	buf := make([]byte, 5)
	n, readErr := serverConn.Read(buf)
	require.NoError(t, readErr)
	assert.Equal(t, "hello", string(buf[:n]))
}

func TestPrivateUnixSocketListenerCloseRemovesSocketFile(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "cls-")
	require.NoError(t, createErr)

	socketPath := listener.SocketPath()

	// Verify socket exists
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr)

	closeErr := listener.Close()
	assert.NoError(t, closeErr)

	// Verify socket was removed
	_, statErr = os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr))

	// Double close should be safe
	closeErr = listener.Close()
	assert.NoError(t, closeErr)
}

func TestPrivateUnixSocketListenerDoesNotRemoveExistingSocketOnCollision(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	// Create listeners that will occupy socket paths in the directory.
	// A new listener should get a different path without removing these.
	l1, err1 := NewPrivateUnixSocketListener(rootDir, "col-")
	require.NoError(t, err1)
	defer l1.Close()

	l2, err2 := NewPrivateUnixSocketListener(rootDir, "col-")
	require.NoError(t, err2)
	defer l2.Close()

	// The first listener's socket must still exist (not removed by the second).
	_, statErr := os.Stat(l1.SocketPath())
	assert.NoError(t, statErr, "existing socket file should not be removed on collision")

	// Both listeners should have distinct paths.
	assert.NotEqual(t, l1.SocketPath(), l2.SocketPath())

	// Both should accept connections.
	for _, listener := range []*PrivateUnixSocketListener{l1, l2} {
		conn, dialErr := net.Dial("unix", listener.SocketPath())
		require.NoError(t, dialErr)
		conn.Close()
	}
}

func TestPrivateUnixSocketListenerConcurrentCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "ccl-")
	require.NoError(t, createErr)

	// Start Accept() in a goroutine; it will block until the listener is closed.
	var acceptErr error
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		_, acceptErr = listener.Accept()
	}()

	// Give the accept goroutine a moment to enter the blocking Accept() call.
	runtime.Gosched()

	// Launch 10 goroutines that all race to call Close().
	const closerCount = 10
	closeErrs := make([]error, closerCount)
	startCh := make(chan struct{})
	var closeWg sync.WaitGroup
	closeWg.Add(closerCount)
	for i := range closerCount {
		go func() {
			defer closeWg.Done()
			<-startCh
			closeErrs[i] = listener.Close()
		}()
	}

	// Signal all closers to race.
	close(startCh)
	closeWg.Wait()

	// Wait for Accept() to return.
	<-acceptDone

	// Accept() must return net.ErrClosed so the caller can distinguish
	// a graceful shutdown from an unexpected error.
	assert.ErrorIs(t, acceptErr, net.ErrClosed)

	// All Close() calls must succeed (Close is idempotent).
	for i, closeErr := range closeErrs {
		assert.NoError(t, closeErr, "Close() call %d returned an error", i)
	}
}

func TestPrivateUnixSocketListenerAddrReturnsValidAddress(t *testing.T) {
	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "addr-")
	require.NoError(t, createErr)
	defer listener.Close()

	addr := listener.Addr()
	require.NotNil(t, addr)
	assert.Equal(t, "unix", addr.Network())
}

func TestPrivateUnixSocketListenerSocketFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket file permission check not applicable on Windows")
	}

	t.Parallel()
	rootDir := shortTempDir(t)

	listener, createErr := NewPrivateUnixSocketListener(rootDir, "perm-")
	require.NoError(t, createErr)
	defer listener.Close()

	info, statErr := os.Stat(listener.SocketPath())
	require.NoError(t, statErr)
	// The socket file should have 0600 permissions (best-effort).
	// On some systems the kernel may adjust socket permissions, so
	// we check that at minimum the group/other write bits are not set.
	perm := info.Mode().Perm()
	assert.Zero(t, perm&0077, fmt.Sprintf("socket should not be accessible by group/others, got %o", perm))
}
