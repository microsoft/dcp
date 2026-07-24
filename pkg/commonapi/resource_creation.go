/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"errors"
	"sync/atomic"
)

var (
	// ResourceCreationProhibited tracks whether new resource creation is blocked because the API server is shutting down.
	ResourceCreationProhibited    = &atomic.Bool{}
	ErrResourceCreationProhibited = errors.New("new resources cannot be created because the API server is shutting down")
)
