//go:build linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatIdentityTimeLinuxUsesDurationSinceBoot(t *testing.T) {
	t.Parallel()

	identityTime := time.Time{}.Add(1234 * time.Millisecond)

	require.Equal(t, "1234ms-since-boot", FormatIdentityTime(identityTime))
}
