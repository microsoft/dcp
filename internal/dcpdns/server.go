/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package dcpdns implements the small DNS forwarder DCP runs inside a container on the
// Apple container runtime's network. The Apple runtime has no network alias support and
// its gateway DNS serves no records for container names, so DCP points every container it
// creates at this forwarder (via `--dns`). The forwarder answers A-record queries for
// names DCP publishes (container names, network aliases, tunnel proxy aliases) from a
// hosts-format records file, and relays every other query verbatim to the upstream
// resolver (the container network gateway), so regular name resolution keeps working.
package dcpdns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/go-logr/logr"
)

const (
	// Maximum DNS message size the forwarder accepts over UDP. Larger answers from
	// upstream are truncated by the upstream resolver itself (TC bit), prompting
	// clients to retry over TCP.
	maxUDPMessageSize = 4096

	// How long to wait for the upstream resolver.
	upstreamTimeout = 5 * time.Second
)

type Server struct {
	records  *Records
	upstream string // host:port of the upstream resolver
	log      logr.Logger
}

func NewServer(records *Records, upstream string, log logr.Logger) *Server {
	return &Server{
		records:  records,
		upstream: upstream,
		log:      log,
	}
}

// Serve answers DNS queries on the given UDP and TCP listeners until the context is
// canceled. Either listener may be nil.
func (s *Server) Serve(ctx context.Context, udpConn net.PacketConn, tcpListener net.Listener) error {
	go func() {
		<-ctx.Done()
		if udpConn != nil {
			_ = udpConn.Close()
		}
		if tcpListener != nil {
			_ = tcpListener.Close()
		}
	}()

	errCh := make(chan error, 2)

	if udpConn != nil {
		go func() { errCh <- s.serveUDP(ctx, udpConn) }()
	}
	if tcpListener != nil {
		go func() { errCh <- s.serveTCP(ctx, tcpListener) }()
	}

	err := <-errCh
	if ctx.Err() != nil {
		return nil // Shutting down
	}
	return err
}

func (s *Server) serveUDP(ctx context.Context, conn net.PacketConn) error {
	buffer := make([]byte, maxUDPMessageSize)
	for {
		length, client, readErr := conn.ReadFrom(buffer)
		if readErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading UDP query: %w", readErr)
		}

		query := make([]byte, length)
		copy(query, buffer[:length])

		go func() {
			response := s.handleQuery(query, s.relayUDP)
			if response != nil {
				if _, writeErr := conn.WriteTo(response, client); writeErr != nil && ctx.Err() == nil {
					s.log.V(1).Info("Failed to write DNS response", "Client", client.String(), "Error", writeErr.Error())
				}
			}
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, listener net.Listener) error {
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting TCP connection: %w", acceptErr)
		}

		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(upstreamTimeout))

			query, readErr := readTCPMessage(conn)
			if readErr != nil {
				return
			}

			response := s.handleQuery(query, s.relayTCP)
			if response != nil {
				_ = writeTCPMessage(conn, response)
			}
		}()
	}
}

// handleQuery answers the query authoritatively when its name is known, and relays it to
// the upstream resolver otherwise. It returns nil if no response should be sent.
func (s *Server) handleQuery(query []byte, relay func([]byte) ([]byte, error)) []byte {
	question, parseErr := ParseQuestion(query)
	if parseErr != nil {
		// Not a query the forwarder understands; let the upstream resolver decide.
		response, relayErr := relay(query)
		if relayErr != nil {
			return nil
		}
		return response
	}

	if addrs, known := s.records.Lookup(question.Name); known {
		// The forwarder is authoritative for this name. Non-A queries (e.g. AAAA) get an
		// empty NOERROR answer so clients fall back to the A records instead of receiving
		// an upstream NXDOMAIN for a name that does exist here.
		var answers []netip.Addr
		if question.Type == TypeA && question.Class == ClassINET {
			answers = addrs
		}
		return BuildResponse(query, question, answers)
	}

	response, relayErr := relay(query)
	if relayErr != nil {
		s.log.V(1).Info("Failed to relay DNS query upstream", "Name", question.Name, "Upstream", s.upstream, "Error", relayErr.Error())
		return nil
	}
	return response
}

func (s *Server) relayUDP(query []byte) ([]byte, error) {
	conn, dialErr := net.DialTimeout("udp", s.upstream, upstreamTimeout)
	if dialErr != nil {
		return nil, dialErr
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(upstreamTimeout))

	if _, writeErr := conn.Write(query); writeErr != nil {
		return nil, writeErr
	}

	response := make([]byte, maxUDPMessageSize)
	length, readErr := conn.Read(response)
	if readErr != nil {
		return nil, readErr
	}

	return response[:length], nil
}

func (s *Server) relayTCP(query []byte) ([]byte, error) {
	conn, dialErr := net.DialTimeout("tcp", s.upstream, upstreamTimeout)
	if dialErr != nil {
		return nil, dialErr
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(upstreamTimeout))

	if writeErr := writeTCPMessage(conn, query); writeErr != nil {
		return nil, writeErr
	}

	return readTCPMessage(conn)
}

// readTCPMessage reads a DNS message in TCP framing (2-byte length prefix, RFC 1035 4.2.2).
func readTCPMessage(conn net.Conn) ([]byte, error) {
	var lengthPrefix [2]byte
	if _, readErr := io.ReadFull(conn, lengthPrefix[:]); readErr != nil {
		return nil, readErr
	}

	length := binary.BigEndian.Uint16(lengthPrefix[:])
	if length == 0 {
		return nil, errors.New("zero-length DNS message")
	}

	message := make([]byte, length)
	if _, readErr := io.ReadFull(conn, message); readErr != nil {
		return nil, readErr
	}

	return message, nil
}

func writeTCPMessage(conn net.Conn, message []byte) error {
	framed := make([]byte, 2+len(message))
	binary.BigEndian.PutUint16(framed[0:2], uint16(len(message)))
	copy(framed[2:], message)

	_, writeErr := conn.Write(framed)
	return writeErr
}
