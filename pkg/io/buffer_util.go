/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"bytes"
)

const (
	// 1 MB. If the internal buffer used by one of our reader or writer types grows larger than this,
	// it will be recycled.
	bufferRecycleThreshold = 1024 * 1024
)

func reset(b **bytes.Buffer) {
	(*b).Reset()
	if (*b).Cap() > bufferRecycleThreshold {
		*b = new(bytes.Buffer)
	}
}
