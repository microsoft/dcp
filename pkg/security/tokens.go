/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

import (
	"github.com/microsoft/dcp/pkg/randdata"
)

const BearerTokenLength = 32

func MakeBearerToken() ([]byte, error) {
	return randdata.MakeRandomString(BearerTokenLength)
}
