/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookupCertificate_NotSupportedOnNonWindows(t *testing.T) {
	_, err := LookupCertificate("aabbccdd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "only supported on Windows")
}
