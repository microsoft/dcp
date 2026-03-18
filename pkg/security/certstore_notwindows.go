/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package security

import "fmt"

func lookupCertificate(_ string) (*ServerCertificateData, string, error) {
	return nil, "", fmt.Errorf("certificate store lookup is only supported on Windows")
}
