/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package openssl

import (
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"unicode"
)

func SubjectHash(cert *x509.Certificate) (string, error) {
	var subject pkix.RDNSequence
	if _, err := asn1.UnmarshalWithParams(cert.RawSubject, &subject, "utf8"); err != nil {
		return "", fmt.Errorf("unable to unmarshal the certificate subject: %w", err)
	}

	hash := sha1.New()

	for i := range subject {
		for j := range subject[i] {
			// All strings are normalized to UTF8 for canonicalization
			if subjectValue, ok := subject[i][j].Value.(string); ok {
				subject[i][j].Value = CanonicalName(subjectValue)
			}
		}

		subjectBytes, marshalErr := asn1.Marshal(subject[i])
		if marshalErr != nil {
			return "", fmt.Errorf("unable to marshal normalized subject segment for certificate: %w", marshalErr)
		}

		// Go doesn't preserve the original tag information when unmarshalling and when re-marshalling
		// strings it applies a tag based on the actual contents of the string; UTF8 if any UTF8 characters are present,
		// PrintableString otherwise. However, OpenSSL converts all string values to UTF8 tags before computing the
		// subject hash, so we need to do the same.
		if len(subjectBytes) > 5 && subjectBytes[4] == asn1.TagOID {
			// Try to parse this as an OID tag
			start := 6 + int(subjectBytes[5])
			if len(subjectBytes) > start && subjectBytes[start] == asn1.TagPrintableString {
				// OpenSSL normalizes all string tags to UTF8 for the purposes of computing the subject hash
				subjectBytes[start] = asn1.TagUTF8String
			}
		}

		if _, writeErr := hash.Write(subjectBytes); writeErr != nil {
			return "", fmt.Errorf("failed to hash the normalized subject for certificate: %w", writeErr)
		}
	}

	subjectHash := hash.Sum(nil)
	numericHash := (uint32(subjectHash[0]) | uint32(subjectHash[1])<<8 | uint32(subjectHash[2])<<16 | uint32(subjectHash[3])<<24) & 0xffffffff
	return fmt.Sprintf("%08x", numericHash), nil
}

func CanonicalName(name string) string {
	var normalizedName string

	outputSpace := false
	trim := true
	for _, c := range name {
		if c > unicode.MaxASCII {
			// Non-ASCII characters are copied as-is
			if outputSpace {
				// We saw one or more spaces before this character, so output a single space
				normalizedName += " "
				outputSpace = false
			}
			normalizedName += string(c)
			trim = false
		} else if unicode.IsSpace(c) {
			// This is a space character, if we're trimming we ignore it, otherwise we just note
			// that we saw a space character so that we can output a single space later
			outputSpace = !trim
		} else {
			if outputSpace {
				// We saw one or more spaces before this character, so output a single space
				normalizedName += " "
				outputSpace = false
			}
			normalizedName += string(unicode.ToLower(c))
			trim = false
		}
	}

	return normalizedName
}
