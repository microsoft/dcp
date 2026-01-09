/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"time"
)

const (
	defaultIntegrationTestTimeout = 60 * time.Second
	waitPollInterval              = 200 * time.Millisecond
)
