/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpdns

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// Minimal DNS wire-format handling (RFC 1035) for the DCP DNS forwarder.
//
// The forwarder only needs to understand enough of the protocol to answer A-record
// queries for names it knows about; everything else is relayed verbatim to the
// upstream resolver, so no general-purpose DNS library is required.

const (
	dnsHeaderLength = 12

	// DNS TYPE and CLASS values (RFC 1035, section 3.2)
	TypeA     = 1
	TypeAAAA  = 28
	ClassINET = 1

	// Response TTL for records served by the forwarder. Container IPs can change when
	// containers are recreated, so responses should not be cached for long.
	answerTTLSeconds = 10

	// Flag bits within the second header pair (RFC 1035, section 4.1.1)
	flagResponse           = 1 << 15 // QR
	flagAuthoritative      = 1 << 10 // AA
	flagRecursionDesired   = 1 << 8  // RD
	flagRecursionAvailable = 1 << 7  // RA
)

// Question describes the first question of a DNS query message.
type Question struct {
	// Query ID from the message header.
	ID uint16

	// The queried name, lower-cased, without the trailing dot.
	Name string

	// The query TYPE (e.g. TypeA).
	Type uint16

	// The query CLASS (usually ClassINET).
	Class uint16

	// Offset of the first byte past the question section entry within the message.
	// Used to copy the question section verbatim into a response.
	EndOffset int

	// Whether the query requested recursion.
	RecursionDesired bool
}

// ParseQuestion extracts the first question from a DNS query message.
// It returns an error for messages that are not plain queries with at least one question.
func ParseQuestion(msg []byte) (Question, error) {
	if len(msg) < dnsHeaderLength {
		return Question{}, fmt.Errorf("message too short: %d bytes", len(msg))
	}

	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&flagResponse != 0 {
		return Question{}, fmt.Errorf("message is a response, not a query")
	}

	questionCount := binary.BigEndian.Uint16(msg[4:6])
	if questionCount == 0 {
		return Question{}, fmt.Errorf("query contains no questions")
	}

	var labels []string
	offset := dnsHeaderLength
	for {
		if offset >= len(msg) {
			return Question{}, fmt.Errorf("question name extends past end of message")
		}

		labelLength := int(msg[offset])
		if labelLength&0xC0 != 0 {
			// Compression pointers are not legal in the question name of a query.
			return Question{}, fmt.Errorf("unexpected compression pointer in question name")
		}
		offset++

		if labelLength == 0 {
			break
		}
		if offset+labelLength > len(msg) {
			return Question{}, fmt.Errorf("question label extends past end of message")
		}

		labels = append(labels, strings.ToLower(string(msg[offset:offset+labelLength])))
		offset += labelLength
	}

	if offset+4 > len(msg) {
		return Question{}, fmt.Errorf("question type/class extends past end of message")
	}

	question := Question{
		ID:               binary.BigEndian.Uint16(msg[0:2]),
		Name:             strings.Join(labels, "."),
		Type:             binary.BigEndian.Uint16(msg[offset : offset+2]),
		Class:            binary.BigEndian.Uint16(msg[offset+2 : offset+4]),
		EndOffset:        offset + 4,
		RecursionDesired: flags&flagRecursionDesired != 0,
	}

	return question, nil
}

// BuildResponse builds an authoritative response to the given query for the provided
// IPv4 addresses. An empty address list produces a NOERROR response with no answers,
// which is the correct authoritative reply for a known name queried with a type the
// forwarder has no records for (e.g. AAAA): the client then falls back to other types
// instead of treating the name as nonexistent.
func BuildResponse(query []byte, question Question, addrs []netip.Addr) []byte {
	response := make([]byte, 0, question.EndOffset+len(addrs)*16)

	// Header
	var header [dnsHeaderLength]byte
	binary.BigEndian.PutUint16(header[0:2], question.ID)
	flags := uint16(flagResponse | flagAuthoritative | flagRecursionAvailable)
	if question.RecursionDesired {
		flags |= flagRecursionDesired
	}
	binary.BigEndian.PutUint16(header[2:4], flags)
	binary.BigEndian.PutUint16(header[4:6], 1)                  // QDCOUNT
	binary.BigEndian.PutUint16(header[6:8], uint16(len(addrs))) // ANCOUNT
	response = append(response, header[:]...)

	// Question section, copied verbatim from the query
	response = append(response, query[dnsHeaderLength:question.EndOffset]...)

	// Answer records: a compression pointer to the question name, followed by an A record
	for _, addr := range addrs {
		ip4 := addr.As4()
		var rr [16]byte
		rr[0] = 0xC0
		rr[1] = dnsHeaderLength // pointer to the question name
		binary.BigEndian.PutUint16(rr[2:4], TypeA)
		binary.BigEndian.PutUint16(rr[4:6], ClassINET)
		binary.BigEndian.PutUint32(rr[6:10], answerTTLSeconds)
		binary.BigEndian.PutUint16(rr[10:12], 4) // RDLENGTH
		copy(rr[12:16], ip4[:])
		response = append(response, rr[:]...)
	}

	return response
}
