/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUniqueNameIsRandomAndDNSCompatible(t *testing.T) {
	t.Parallel()

	first := UniqueName(t, "Test/Container_Name")
	second := UniqueName(t, "Test/Container_Name")

	require.NotEqual(t, first, second)
	require.LessOrEqual(t, len(first), maxResourceNameLength)
	require.True(t, regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(first))
	require.True(t, strings.HasPrefix(first, "test-container-name-"))
}
