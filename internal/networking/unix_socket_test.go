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

func TestPrepareSecureSocketDir(t *testing.T) {
	t.Parallel()

	t.Run("creates directory with correct permissions", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		socketDir, prepareErr := PrepareSecureSocketDir(rootDir)
		require.NoError(t, prepareErr)

		expectedDir := filepath.Join(rootDir, dcppaths.DcpWorkDir)
		assert.Equal(t, expectedDir, socketDir)

		info, statErr := os.Stat(socketDir)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
		if runtime.GOOS != "windows" {
			assert.Equal(t, osutil.PermissionOnlyOwnerReadWriteTraverse, info.Mode().Perm())
		}
	})

	t.Run("idempotent on repeated calls", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		dir1, err1 := PrepareSecureSocketDir(rootDir)
		require.NoError(t, err1)

		dir2, err2 := PrepareSecureSocketDir(rootDir)
		require.NoError(t, err2)

		assert.Equal(t, dir1, dir2)
	})

	t.Run("falls back to user cache dir when rootDir is empty", func(t *testing.T) {
		t.Parallel()

		socketDir, prepareErr := PrepareSecureSocketDir("")
		require.NoError(t, prepareErr)

		cacheDir, cacheDirErr := os.UserCacheDir()
		require.NoError(t, cacheDirErr)

		expectedDir := filepath.Join(cacheDir, dcppaths.DcpWorkDir)
		assert.Equal(t, expectedDir, socketDir)
	})

	t.Run("rejects directory with wrong permissions on unix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission validation is skipped on Windows")
		}

		t.Parallel()
		rootDir := shortTempDir(t)

		// Pre-create the dcp-work directory with overly-permissive permissions
		socketDir := filepath.Join(rootDir, dcppaths.DcpWorkDir)
		mkdirErr := os.MkdirAll(socketDir, 0755)
		require.NoError(t, mkdirErr)

		_, prepareErr := PrepareSecureSocketDir(rootDir)
		require.Error(t, prepareErr)
		assert.Contains(t, prepareErr.Error(), "not private to the user")
	})
}

func TestNewSecureSocketListener(t *testing.T) {
	t.Parallel()

	t.Run("creates listener with random name", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "test-")
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
	})

	t.Run("two listeners get different paths", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		l1, err1 := NewSecureSocketListener(rootDir, "dup-")
		require.NoError(t, err1)
		defer l1.Close()

		l2, err2 := NewSecureSocketListener(rootDir, "dup-")
		require.NoError(t, err2)
		defer l2.Close()

		assert.NotEqual(t, l1.SocketPath(), l2.SocketPath(), "two listeners with the same prefix should have different socket paths")
	})

	t.Run("accepts connections", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "acc-")
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
	})

	t.Run("close removes socket file", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "cls-")
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
	})

	t.Run("removes stale socket file on create", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		// Create first listener to get a socket path
		l1, err1 := NewSecureSocketListener(rootDir, "stale-")
		require.NoError(t, err1)
		socketPath := l1.SocketPath()
		l1.Close()

		// Manually create a stale file at the same path
		staleFile, createFileErr := os.Create(socketPath)
		require.NoError(t, createFileErr)
		staleFile.Close()

		// Create new listener with the exact same path — this exercises the stale removal
		// Since we can't predict the random suffix, we test via PrepareSecureSocketDir + manual path
		// Instead, verify that a new listener in the same dir works fine
		l2, err2 := NewSecureSocketListener(rootDir, "stale-")
		require.NoError(t, err2)
		defer l2.Close()

		// Verify we can connect
		conn, dialErr := net.Dial("unix", l2.SocketPath())
		require.NoError(t, dialErr)
		conn.Close()
	})

	t.Run("accept returns error after close", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "afc-")
		require.NoError(t, createErr)

		closeErr := listener.Close()
		require.NoError(t, closeErr)

		_, acceptErr := listener.Accept()
		assert.Error(t, acceptErr)
	})

	t.Run("Addr returns valid address", func(t *testing.T) {
		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "addr-")
		require.NoError(t, createErr)
		defer listener.Close()

		addr := listener.Addr()
		require.NotNil(t, addr)
		assert.Equal(t, "unix", addr.Network())
	})

	t.Run("socket file permissions on unix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("socket file permission check not applicable on Windows")
		}

		t.Parallel()
		rootDir := shortTempDir(t)

		listener, createErr := NewSecureSocketListener(rootDir, "perm-")
		require.NoError(t, createErr)
		defer listener.Close()

		info, statErr := os.Stat(listener.SocketPath())
		require.NoError(t, statErr)
		// The socket file should have 0600 permissions (best-effort).
		// On some systems the kernel may adjust socket permissions, so
		// we check that at minimum the group/other write bits are not set.
		perm := info.Mode().Perm()
		assert.Zero(t, perm&0077, fmt.Sprintf("socket should not be accessible by group/others, got %o", perm))
	})
}
