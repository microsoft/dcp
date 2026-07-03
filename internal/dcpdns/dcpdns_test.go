/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpdns

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// buildQuery constructs a DNS query message for the given name and type.
func buildQuery(t *testing.T, id uint16, name string, qtype uint16) []byte {
	t.Helper()

	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], flagRecursionDesired)
	binary.BigEndian.PutUint16(msg[4:6], 1) // QDCOUNT

	for start := 0; start < len(name); {
		end := start
		for end < len(name) && name[end] != '.' {
			end++
		}
		require.Greater(t, end, start, "empty label in %q", name)
		msg = append(msg, byte(end-start))
		msg = append(msg, name[start:end]...)
		start = end + 1
	}
	msg = append(msg, 0)

	var typeAndClass [4]byte
	binary.BigEndian.PutUint16(typeAndClass[0:2], qtype)
	binary.BigEndian.PutUint16(typeAndClass[2:4], ClassINET)
	return append(msg, typeAndClass[:]...)
}

func writeRecordsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestParseQuestion(t *testing.T) {
	t.Parallel()

	query := buildQuery(t, 0x1234, "MyRedis.Example", TypeA)
	question, err := ParseQuestion(query)
	require.NoError(t, err)
	require.EqualValues(t, 0x1234, question.ID)
	require.Equal(t, "myredis.example", question.Name, "names should be lower-cased")
	require.EqualValues(t, TypeA, question.Type)
	require.EqualValues(t, ClassINET, question.Class)
	require.Equal(t, len(query), question.EndOffset)
	require.True(t, question.RecursionDesired)
}

func TestParseQuestionRejectsResponsesAndGarbage(t *testing.T) {
	t.Parallel()

	_, err := ParseQuestion([]byte{0x00, 0x01, 0x02})
	require.Error(t, err, "truncated message should be rejected")

	response := buildQuery(t, 1, "example.com", TypeA)
	binary.BigEndian.PutUint16(response[2:4], flagResponse)
	_, err = ParseQuestion(response)
	require.Error(t, err, "a response message should be rejected")
}

func TestBuildResponseAnswersARecords(t *testing.T) {
	t.Parallel()

	query := buildQuery(t, 42, "myredis", TypeA)
	question, err := ParseQuestion(query)
	require.NoError(t, err)

	addr := netip.MustParseAddr("192.168.64.7")
	response := BuildResponse(query, question, []netip.Addr{addr})

	require.EqualValues(t, 42, binary.BigEndian.Uint16(response[0:2]), "response ID must match the query")
	flags := binary.BigEndian.Uint16(response[2:4])
	require.NotZero(t, flags&flagResponse, "QR bit must be set")
	require.NotZero(t, flags&flagAuthoritative, "AA bit must be set")
	require.Zero(t, flags&0x000F, "RCODE must be NOERROR")
	require.EqualValues(t, 1, binary.BigEndian.Uint16(response[6:8]), "one answer expected")

	// The answer's RDATA is the last 4 bytes
	require.Equal(t, addr.As4(), [4]byte(response[len(response)-4:]))
}

func TestBuildResponseEmptyAnswerForUnservedType(t *testing.T) {
	t.Parallel()

	query := buildQuery(t, 7, "myredis", TypeAAAA)
	question, err := ParseQuestion(query)
	require.NoError(t, err)

	response := BuildResponse(query, question, nil)
	flags := binary.BigEndian.Uint16(response[2:4])
	require.Zero(t, flags&0x000F, "RCODE must be NOERROR (not NXDOMAIN) so clients fall back to A")
	require.EqualValues(t, 0, binary.BigEndian.Uint16(response[6:8]), "no answers expected")
}

func TestRecordsLookupAndReload(t *testing.T) {
	t.Parallel()

	path := writeRecordsFile(t, "192.168.64.5 myredis myredis.dcp # a comment\nbogus line\n")
	records := NewRecords(path)

	addrs, found := records.Lookup("MyRedis.")
	require.True(t, found)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("192.168.64.5")}, addrs)

	_, found = records.Lookup("unknown")
	require.False(t, found)

	// Rewrite the file with a future mod time and verify the new content is served
	require.NoError(t, os.WriteFile(path, []byte("192.168.64.9 postgres\n"), 0644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	_, found = records.Lookup("myredis")
	require.False(t, found, "removed records should disappear after reload")
	addrs, found = records.Lookup("postgres")
	require.True(t, found)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("192.168.64.9")}, addrs)
}

// TestServerEndToEnd runs the forwarder on loopback with a fake upstream resolver and
// verifies both the authoritative and the relay paths over UDP.
func TestServerEndToEnd(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	// Fake upstream: replies to any query with a fixed A record response
	upstream, upstreamErr := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, upstreamErr)
	defer upstream.Close()
	upstreamAnswer := netip.MustParseAddr("203.0.113.99")
	go func() {
		buffer := make([]byte, maxUDPMessageSize)
		for {
			length, client, readErr := upstream.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			query := make([]byte, length)
			copy(query, buffer[:length])
			if question, parseErr := ParseQuestion(query); parseErr == nil {
				_, _ = upstream.WriteTo(BuildResponse(query, question, []netip.Addr{upstreamAnswer}), client)
			}
		}
	}()

	recordsPath := writeRecordsFile(t, "192.168.64.5 myredis\n")

	serverConn, listenErr := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, listenErr)

	server := NewServer(NewRecords(recordsPath), upstream.LocalAddr().String(), logr.Discard())
	serveCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	go func() { _ = server.Serve(serveCtx, serverConn, nil) }()

	resolve := func(name string, qtype uint16) []byte {
		conn, dialErr := net.Dial("udp", serverConn.LocalAddr().String())
		require.NoError(t, dialErr)
		defer conn.Close()
		require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

		_, writeErr := conn.Write(buildQuery(t, 99, name, qtype))
		require.NoError(t, writeErr)

		response := make([]byte, maxUDPMessageSize)
		length, readErr := conn.Read(response)
		require.NoError(t, readErr)
		return response[:length]
	}

	// Known name: answered authoritatively from the records file
	response := resolve("myredis", TypeA)
	require.EqualValues(t, 1, binary.BigEndian.Uint16(response[6:8]))
	require.Equal(t, netip.MustParseAddr("192.168.64.5").As4(), [4]byte(response[len(response)-4:]))

	// Known name, AAAA: empty NOERROR (not relayed upstream)
	response = resolve("myredis", TypeAAAA)
	require.EqualValues(t, 0, binary.BigEndian.Uint16(response[6:8]))

	// Unknown name: relayed to the upstream resolver
	response = resolve("example.com", TypeA)
	require.EqualValues(t, 1, binary.BigEndian.Uint16(response[6:8]))
	require.Equal(t, upstreamAnswer.As4(), [4]byte(response[len(response)-4:]))
}
