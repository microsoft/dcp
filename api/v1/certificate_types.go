/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/microsoft/dcp/internal/openssl"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// PemCertificate represents a PEM encoded (public) certificate
// +k8s:openapi-gen=true
type PemCertificate struct {
	// The certificate thumbprint
	Thumbprint string `json:"thumbprint"`

	// The PEM encoded certificate data
	Contents string `json:"contents"`
}

func (pc *PemCertificate) Equal(other *PemCertificate) bool {
	if pc == other {
		return true
	}

	if pc == nil || other == nil {
		return false
	}

	if pc.Thumbprint != other.Thumbprint {
		return false
	}

	if pc.Contents != other.Contents {
		return false
	}

	return true
}

func (pc *PemCertificate) Validate(fieldPath *field.Path) field.ErrorList {
	if pc == nil {
		return nil
	}

	var errorList field.ErrorList

	if pc.Thumbprint == "" {
		errorList = append(errorList, field.Required(fieldPath.Child("thumbprint"), "thumbprint must be set to a non-empty value"))
	}

	if pc.Contents == "" {
		errorList = append(errorList, field.Required(fieldPath.Child("contents"), "contents must be set to a non-empty value"))
	}

	return errorList
}

// ToX509Certificate converts the PemCertificate to an x509.Certificate
func (pc *PemCertificate) ToX509Certificate() (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pc.Contents))
	if block == nil {
		return nil, fmt.Errorf("could not decode PEM block from certificate %s", pc.Thumbprint)
	} else if block.Type != "CERTIFICATE" {
		// Expected a PEM encoded public certificate, but received something else
		return nil, fmt.Errorf("expected a single PEM format public certificate for %s, but received something else: %s", pc.Thumbprint, block.Type)
	}

	certBytes := block.Bytes

	x509Cert, certErr := x509.ParseCertificate(certBytes)
	if certErr != nil {
		return nil, fmt.Errorf("could not parse certificate %s: %w", pc.Thumbprint, certErr)
	}

	return x509Cert, nil
}

// OpenSSLFingerprint computes the OpenSSL-style subject hash for the certificate
func (pc *PemCertificate) OpenSSLFingerprint() (string, error) {
	x509Cert, err := pc.ToX509Certificate()
	if err != nil {
		return "", err
	}

	return openssl.SubjectHash(x509Cert)
}
