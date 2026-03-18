/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package security

// LookupCertificate looks up a certificate by its SHA-1 thumbprint in the
// system certificate store (CurrentUser\My on Windows) and returns the
// certificate data including the private key, along with the validated
// server address the certificate covers.
// This is only supported on Windows; on other platforms it returns an error.
func LookupCertificate(thumbprint string) (*ServerCertificateData, string, error) {
	return lookupCertificate(thumbprint)
}
