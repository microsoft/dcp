//go:build !linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
)

func processDisplayTime(info processInfo) (time.Time, error) {
	return info.handle.IdentityTime, nil
}

func formatIdentityTime(identityTime time.Time) string {
	return identityTime.Format(osutil.RFC3339MiliTimestampFormat)
}
