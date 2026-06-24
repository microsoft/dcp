/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/randdata"
)

const (
	socketPathRandomSuffixLength       = 8
	randomSocketListenerCreateAttempts = 16
)

var errUnixSocketPathExists = errors.New("unix socket path already exists")

// UnixSocketListener owns a Unix domain socket listener and its socket file.
type UnixSocketListener struct {
	listener        *net.UnixListener
	socketPath      string
	lock            *sync.Mutex
	closed          bool
	closeErrPromise *concurrency.ValuePromise[error]
}

var _ net.Listener = (*UnixSocketListener)(nil)

// CreateRandomSocketPath builds a unique Unix domain socket path under a
// per-program subfolder, creating that subfolder (private to the current user)
// as necessary. The returned path is not bound; the caller is responsible for
// creating and removing the socket file.
//
// If rootDir is empty, the user's cache directory is used. The subfolder name
// is derived from the running executable's name with its extension stripped
// (so both "dcp" and "dcp.exe" map to "dcp"). socketNamePrefix is prepended to
// a random suffix to form the socket file name.
//
// The function fails if the resulting path would exceed MaxUnixSocketPathLen,
// since such a path cannot be used for an AF_UNIX socket.
func CreateRandomSocketPath(rootDir string, socketNamePrefix string) (string, error) {
	socketDir, socketDirErr := prepareSocketDir(rootDir)
	if socketDirErr != nil {
		return "", socketDirErr
	}

	return randomSocketPath(socketDir, socketNamePrefix)
}

// CreateRandomUnixSocketListener creates and binds a Unix domain socket listener
// at a unique path under a private per-program subfolder. The returned listener
// owns the socket file and removes it when closed.
func CreateRandomUnixSocketListener(rootDir string, socketNamePrefix string) (*UnixSocketListener, error) {
	socketDir, socketDirErr := prepareSocketDir(rootDir)
	if socketDirErr != nil {
		return nil, socketDirErr
	}

	var lastErr error
	for range randomSocketListenerCreateAttempts {
		socketPath, pathErr := randomSocketPath(socketDir, socketNamePrefix)
		if pathErr != nil {
			return nil, pathErr
		}

		listener, listenerErr := CreateUnixSocketListener(socketPath)
		if listenerErr == nil {
			return listener, nil
		}
		if !errors.Is(listenerErr, errUnixSocketPathExists) {
			return nil, listenerErr
		}
		lastErr = listenerErr
	}

	return nil, fmt.Errorf("failed to create a unique Unix socket listener after %d attempts: %w", randomSocketListenerCreateAttempts, lastErr)
}

// CreateUnixSocketListener creates and binds a Unix domain socket listener at
// socketPath. The path must not already exist. The returned listener owns the
// socket file and removes it when closed.
func CreateUnixSocketListener(socketPath string) (*UnixSocketListener, error) {
	if socketPath == "" {
		return nil, errors.New("socket path must not be empty")
	}
	if len(socketPath) > MaxUnixSocketPathLen {
		return nil, fmt.Errorf("socket path %s is %d characters long, which exceeds the maximum of %d for a Unix domain socket", socketPath, len(socketPath), MaxUnixSocketPathLen)
	}

	_, lstatErr := os.Lstat(socketPath)
	if lstatErr == nil {
		return nil, fmt.Errorf("%w: %s", errUnixSocketPathExists, socketPath)
	}
	if !errors.Is(lstatErr, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to inspect Unix socket path %s: %w", socketPath, lstatErr)
	}

	boundListener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if listenErr != nil {
		if errors.Is(listenErr, syscall.EADDRINUSE) || errors.Is(listenErr, syscall.EEXIST) || errors.Is(listenErr, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s: %w", errUnixSocketPathExists, socketPath, listenErr)
		}
		return nil, fmt.Errorf("failed to create Unix socket listener at %s: %w", socketPath, listenErr)
	}

	boundListener.SetUnlinkOnClose(false)

	return &UnixSocketListener{
		listener:        boundListener,
		socketPath:      socketPath,
		lock:            &sync.Mutex{},
		closeErrPromise: concurrency.NewValuePromise[error](),
	}, nil
}

// Accept waits for and returns the next connection to the listener.
func (l *UnixSocketListener) Accept() (net.Conn, error) {
	return l.listener.Accept()
}

// AcceptUnix waits for and returns the next Unix domain connection to the listener.
func (l *UnixSocketListener) AcceptUnix() (*net.UnixConn, error) {
	return l.listener.AcceptUnix()
}

// Close closes the listener and removes the owned socket file. Close is idempotent.
func (l *UnixSocketListener) Close() error {
	l.lock.Lock()

	if l.closed {
		l.lock.Unlock()
		return l.closeErrPromise.Get()
	}

	l.closed = true
	l.lock.Unlock()

	closeErr := l.listener.Close()

	removeErr := os.Remove(l.socketPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}

	closeErr = errors.Join(closeErr, removeErr)
	l.closeErrPromise.Set(closeErr)
	return closeErr
}

// Addr returns the listener's network address.
func (l *UnixSocketListener) Addr() net.Addr {
	return l.listener.Addr()
}

// SocketPath returns the filesystem path of the Unix domain socket.
func (l *UnixSocketListener) SocketPath() string {
	return l.socketPath
}

func prepareSocketDir(rootDir string) (string, error) {
	if rootDir == "" {
		cacheDir, cacheDirErr := os.UserCacheDir()
		if cacheDirErr != nil {
			return "", fmt.Errorf("failed to get user cache directory when creating a Unix socket: %w", cacheDirErr)
		}
		rootDir = cacheDir
	}

	exePath, exePathErr := os.Executable()
	if exePathErr != nil {
		return "", fmt.Errorf("failed to determine the current executable when creating a Unix socket: %w", exePathErr)
	}

	socketDir := filepath.Join(rootDir, programSubfolderName(exePath))
	if err := os.MkdirAll(socketDir, PermissionOnlyOwnerReadWriteTraverse); err != nil {
		return "", fmt.Errorf("failed to create directory for socket: %w", err)
	}

	// On Windows the user cache directory always exists and is always private to the user,
	// but on Unix-like systems, we need to ensure the directory is private.
	if !IsWindows() {
		info, infoErr := os.Stat(socketDir)
		if infoErr != nil {
			return "", fmt.Errorf("failed to check permissions on the socket directory: %w", infoErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("socket path %s is not a directory", socketDir)
		}
		if info.Mode().Perm() != PermissionOnlyOwnerReadWriteTraverse {
			return "", fmt.Errorf("socket directory %s is not private to the user", socketDir)
		}
	}

	return socketDir, nil
}

func randomSocketPath(socketDir string, socketNamePrefix string) (string, error) {
	suffix, suffixErr := randdata.MakeRandomString(socketPathRandomSuffixLength)
	if suffixErr != nil {
		return "", fmt.Errorf("failed to create random string for socket path suffix: %w", suffixErr)
	}

	socketPath := filepath.Join(socketDir, socketNamePrefix+string(suffix))
	if len(socketPath) > MaxUnixSocketPathLen {
		return "", fmt.Errorf("socket path %s is %d characters long, which exceeds the maximum of %d for a Unix domain socket", socketPath, len(socketPath), MaxUnixSocketPathLen)
	}

	return socketPath, nil
}

// programSubfolderName returns the base name of the executable at exePath with
// any file extension removed (e.g. "dcp.exe" and "/usr/bin/dcp" both yield "dcp").
func programSubfolderName(exePath string) string {
	base := filepath.Base(exePath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
