/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestProcessHandle_Comparable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	h1 := NewProcessHandle(Uint32_ToPidT(100), now)
	h2 := NewProcessHandle(Uint32_ToPidT(100), now)
	h3 := NewProcessHandle(Uint32_ToPidT(200), now)

	assert.Equal(t, h1, h2)
	assert.NotEqual(t, h1, h3)

	// Verify usable as map key (replaces WaitKey)
	m := map[ProcessHandle]string{
		h1: "first",
		h3: "second",
	}
	assert.Equal(t, "first", m[h2])
	assert.Equal(t, "second", m[h3])
}
