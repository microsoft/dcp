/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"os"
	"path/filepath"

	"github.com/microsoft/dcp/internal/statestore"
)

type IncludedController uint32

const (
	ExecutableController IncludedController = 1 << iota
	ExecutableReplicaSetController
	NetworkController
	ContainerController
	ContainerExecController
	VolumeController
	ServiceController
	ContainerNetworkTunnelProxyController
	NoControllers  IncludedController = 0
	AllControllers IncludedController = ^NoControllers
)

const (
	NoSeparateWorkingDir = ""
)

func createTestStateStore(ctx context.Context, testTempDir string) (*statestore.Store, func(), error) {
	stateStoreDir := testTempDir
	removeStateStoreDir := false
	if stateStoreDir == NoSeparateWorkingDir {
		tempDir, tempDirErr := os.MkdirTemp("", "dcp-state-store-*")
		if tempDirErr != nil {
			return nil, nil, tempDirErr
		}
		stateStoreDir = tempDir
		removeStateStoreDir = true
	}

	stateStorePath := filepath.Join(stateStoreDir, "state.sqlite3")
	stateStore, stateStoreErr := statestore.Open(ctx, statestore.Options{Path: stateStorePath})
	if stateStoreErr != nil {
		if removeStateStoreDir {
			_ = os.RemoveAll(stateStoreDir)
		}
		return nil, nil, stateStoreErr
	}

	cleanup := func() {
		_ = stateStore.Close()
		if removeStateStoreDir {
			_ = os.RemoveAll(stateStoreDir)
		}
	}

	return stateStore, cleanup, nil
}
