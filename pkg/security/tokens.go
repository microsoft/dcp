// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import (
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

const BearerTokenLength = 32

func MakeBearerToken() ([]byte, error) {
	return randdata.MakeRandomString(BearerTokenLength)
}
