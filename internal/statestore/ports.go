/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	ipv4AddressBytes = 4
	ipv6AddressBytes = 16
)

var ErrPortReservationHeld = errors.New("port reservation is held by another owner")

// PortReservation describes a state-store claim on a protocol/IP/port tuple by a DCP owner process.
type PortReservation struct {
	ports.Binding
	OwnerProcess process.ProcessTreeItem
	UpdatedAt    time.Time
}

// PortReservationRequest identifies the protocol/IP/port tuple to reserve or release and the DCP
// owner process responsible for that claim.
type PortReservationRequest struct {
	ports.Binding
	OwnerProcess process.ProcessTreeItem
}

// PortReservationHeldError reports the conflicting active reservation when a requested
// protocol/IP/port tuple is already held by another DCP owner.
type PortReservationHeldError struct {
	Reservation PortReservation
}

func (e *PortReservationHeldError) Error() string {
	return fmt.Sprintf("%s: %s/%s/%d", ErrPortReservationHeld, e.Reservation.Protocol, e.Reservation.IP, e.Reservation.Port)
}

func (e *PortReservationHeldError) Unwrap() error {
	return ErrPortReservationHeld
}

// HeldPortReservation extracts the active reservation that blocked a reserve attempt.
func HeldPortReservation(err error) (*PortReservation, bool) {
	var heldErr *PortReservationHeldError
	if !errors.As(err, &heldErr) {
		return nil, false
	}

	reservation := heldErr.Reservation
	return &reservation, true
}

// CreatePortReservation creates a reservation for the requested protocol/IP/port tuple.
// It fails with ErrPortReservationHeld when any active owner already reserves the same tuple or a
// same-family wildcard tuple for that port.
func (s *Store) CreatePortReservation(ctx context.Context, request PortReservationRequest) (*PortReservation, error) {
	return s.reservePort(ctx, request, false)
}

// CreateOrUpdatePortReservation creates a reservation for the requested protocol/IP/port tuple.
// If the same owner already holds the exact tuple, it refreshes that reservation instead. It still
// rejects wildcard/specific conflicts so one owner cannot hold overlapping claims for the same port.
func (s *Store) CreateOrUpdatePortReservation(ctx context.Context, request PortReservationRequest) (*PortReservation, error) {
	return s.reservePort(ctx, request, true)
}

// ReleasePort removes this owner process' exact protocol/IP/port reservation.
// Reservations held by other owners, or overlapping wildcard/specific reservations, are left intact.
func (s *Store) ReleasePort(ctx context.Context, request PortReservationRequest) error {
	normalizedRequest, requestErr := normalizePortReservationRequest(request)
	if requestErr != nil {
		return requestErr
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		return deleteExactPortReservation(ctx, conn, normalizedRequest)
	})
}

// DeleteInactivePortReservations deletes reservations whose owner process identity is no longer
// active, allowing ports abandoned by exited DCP instances to be reused.
func (s *Store) DeleteInactivePortReservations(ctx context.Context) error {
	candidates, candidatesErr := s.inactivePortReservationCandidates(ctx)
	if candidatesErr != nil {
		return candidatesErr
	}
	if len(candidates) == 0 {
		return nil
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		for _, candidate := range candidates {
			_, execErr := conn.ExecContext(
				ctx,
				`DELETE FROM port_allocations
				 WHERE protocol = ? AND ip = ? AND port = ?
					AND owner_pid = ? AND owner_identity_time = ?
					AND updated_at_unix_nano = ?`,
				candidate.Protocol,
				portReservationIPBytes(candidate.IP),
				candidate.Port,
				candidate.OwnerProcess.Pid,
				timeString(candidate.OwnerProcess.IdentityTime),
				unixNano(candidate.UpdatedAt),
			)
			if execErr != nil {
				return fmt.Errorf("could not delete inactive port reservation '%s/%s/%d': %w", candidate.Protocol, candidate.IP, candidate.Port, execErr)
			}
		}
		return nil
	})
}

func (s *Store) reservePort(ctx context.Context, request PortReservationRequest, updateIfExisting bool) (*PortReservation, error) {
	normalizedRequest, requestErr := normalizePortReservationRequest(request)
	if requestErr != nil {
		return nil, requestErr
	}

	now := time.Now().UTC()
	var reservation *PortReservation
	txErr := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		requestKey := normalizedPortReservationKey{
			protocol: normalizedRequest.Protocol,
			ip:       normalizedRequest.IP,
			ipBytes:  portReservationIPBytes(normalizedRequest.IP),
			port:     normalizedRequest.Port,
		}

		conflictingReservations, conflictsErr := getConflictingPortReservations(ctx, conn, requestKey)
		if conflictsErr != nil {
			return conflictsErr
		}
		if len(conflictingReservations) == 0 {
			insertErr := insertPortReservation(ctx, conn, normalizedRequest, now)
			if insertErr != nil {
				return insertErr
			}
			var readErr error
			reservation, _, readErr = getExactPortReservation(ctx, conn, normalizedRequest.Protocol, normalizedRequest.IP, normalizedRequest.Port)
			return readErr
		}

		inactiveReservations := make([]*PortReservation, 0, len(conflictingReservations))
		shouldUpdateExactReservation := false
		for _, conflictingReservation := range conflictingReservations {
			exactIP := conflictingReservation.IP == normalizedRequest.IP
			sameOwner := conflictingReservation.OwnerProcess.Pid == normalizedRequest.OwnerProcess.Pid &&
				conflictingReservation.OwnerProcess.IdentityTime.Equal(normalizedRequest.OwnerProcess.IdentityTime)
			if updateIfExisting && sameOwner {
				if exactIP {
					shouldUpdateExactReservation = true
					continue
				}
			}
			active := resourceLeaseOwnerIsActive(conflictingReservation.OwnerProcess)
			if !active {
				if exactIP {
					shouldUpdateExactReservation = true
				} else {
					inactiveReservations = append(inactiveReservations, conflictingReservation)
				}
				continue
			}

			return &PortReservationHeldError{Reservation: *conflictingReservation}
		}
		for _, conflictingReservation := range inactiveReservations {
			deleteErr := deletePortReservation(ctx, conn, conflictingReservation)
			if deleteErr != nil {
				return deleteErr
			}
		}
		if shouldUpdateExactReservation {
			updateErr := updatePortReservation(ctx, conn, normalizedRequest, now)
			if updateErr != nil {
				return updateErr
			}
		} else {
			insertErr := insertPortReservation(ctx, conn, normalizedRequest, now)
			if insertErr != nil {
				return insertErr
			}
		}
		var readErr error
		reservation, _, readErr = getExactPortReservation(ctx, conn, normalizedRequest.Protocol, normalizedRequest.IP, normalizedRequest.Port)
		return readErr
	})
	if txErr != nil {
		return nil, txErr
	}

	return reservation, nil
}

func normalizePortReservationRequest(request PortReservationRequest) (PortReservationRequest, error) {
	key, keyErr := normalizePortReservationKey(request.Binding)
	if keyErr != nil {
		return PortReservationRequest{}, keyErr
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(request.OwnerProcess)
	if ownerErr != nil {
		return PortReservationRequest{}, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}

	return PortReservationRequest{
		Binding: ports.Binding{
			Protocol: key.protocol,
			IP:       key.ip,
			Port:     key.port,
		},
		OwnerProcess: normalizedOwner,
	}, nil
}

type normalizedPortReservationKey struct {
	protocol string
	ip       netip.Addr
	ipBytes  []byte
	port     int32
}

func normalizePortReservationKey(binding ports.Binding) (normalizedPortReservationKey, error) {
	protocol := strings.ToUpper(strings.TrimSpace(binding.Protocol))
	if protocol == "" {
		return normalizedPortReservationKey{}, fmt.Errorf("%w: port reservation protocol cannot be empty", ErrInvalidArgument)
	}
	if !binding.IP.IsValid() {
		return normalizedPortReservationKey{}, fmt.Errorf("%w: port reservation ip cannot be empty", ErrInvalidArgument)
	}
	if !ports.IsValidPort(int(binding.Port)) {
		return normalizedPortReservationKey{}, fmt.Errorf("%w: port reservation port must be between 1 and 65535", ErrInvalidArgument)
	}
	addr := binding.IP.Unmap()
	return normalizedPortReservationKey{
		protocol: protocol,
		ip:       addr,
		ipBytes:  portReservationIPBytes(addr),
		port:     binding.Port,
	}, nil
}

func portReservationIPBytes(addr netip.Addr) []byte {
	if addr.Is4() {
		bytes := addr.As4()
		return append([]byte(nil), bytes[:]...)
	}
	bytes := addr.As16()
	return append([]byte(nil), bytes[:]...)
}

func portReservationAddrFromBytes(ipBytes []byte) (netip.Addr, error) {
	switch len(ipBytes) {
	case ipv4AddressBytes:
		var bytes [ipv4AddressBytes]byte
		copy(bytes[:], ipBytes)
		return netip.AddrFrom4(bytes), nil
	case ipv6AddressBytes:
		var bytes [ipv6AddressBytes]byte
		copy(bytes[:], ipBytes)
		return netip.AddrFrom16(bytes), nil
	default:
		return netip.Addr{}, fmt.Errorf("port reservation ip has invalid byte length %d", len(ipBytes))
	}
}

type portReservationScanner interface {
	Scan(dest ...any) error
}

type portReservationQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getExactPortReservation(ctx context.Context, querier portReservationQuerier, protocol string, ip netip.Addr, port int32) (*PortReservation, bool, error) {
	key, keyErr := normalizePortReservationKey(ports.Binding{Protocol: protocol, IP: ip, Port: port})
	if keyErr != nil {
		return nil, false, keyErr
	}
	row := querier.QueryRowContext(
		ctx,
		`SELECT protocol, ip, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM port_allocations
		 WHERE protocol = ? AND ip = ? AND port = ?`,
		key.protocol,
		key.ipBytes,
		key.port,
	)

	reservation, scanErr := scanPortReservation(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, false, nil
	}
	if scanErr != nil {
		return nil, false, fmt.Errorf("could not read port reservation '%s/%s/%d': %w", key.protocol, key.ip, key.port, scanErr)
	}
	return reservation, true, nil
}

func getConflictingPortReservations(ctx context.Context, querier portReservationQuerier, key normalizedPortReservationKey) ([]*PortReservation, error) {
	rows, queryErr := querier.QueryContext(
		ctx,
		`SELECT protocol, ip, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM port_allocations
		 WHERE protocol = ? AND port = ?`,
		key.protocol,
		key.port,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list port reservations for '%s/%s/%d': %w", key.protocol, key.ip, key.port, queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	reservations := []*PortReservation{}
	for rows.Next() {
		reservation, scanErr := scanPortReservation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read port reservation for '%s/%s/%d': %w", key.protocol, key.ip, key.port, scanErr)
		}
		if portReservationIPsConflict(key.ipBytes, portReservationIPBytes(reservation.IP)) {
			reservations = append(reservations, reservation)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list port reservations for '%s/%s/%d': %w", key.protocol, key.ip, key.port, rowsErr)
	}

	return reservations, nil
}

func portReservationIPsConflict(requestedIP []byte, reservedIP []byte) bool {
	if bytes.Equal(requestedIP, reservedIP) {
		return true
	}
	if len(requestedIP) != len(reservedIP) {
		return false
	}

	requestedAddr, requestedErr := portReservationAddrFromBytes(requestedIP)
	if requestedErr != nil {
		return false
	}
	reservedAddr, reservedErr := portReservationAddrFromBytes(reservedIP)
	if reservedErr != nil {
		return false
	}

	return requestedAddr.IsUnspecified() || reservedAddr.IsUnspecified()
}

func scanPortReservation(row portReservationScanner) (*PortReservation, error) {
	var reservation PortReservation
	var ipBytes []byte
	var port int64
	var ownerPID int64
	var ownerIdentityTime string
	var updatedAtUnixNano int64

	scanErr := row.Scan(
		&reservation.Protocol,
		&ipBytes,
		&port,
		&ownerPID,
		&ownerIdentityTime,
		&updatedAtUnixNano,
	)
	if scanErr != nil {
		return nil, scanErr
	}
	if !ports.IsValidPort(int(port)) {
		return nil, fmt.Errorf("port reservation port is out of range: %d", port)
	}
	ip, ipErr := portReservationAddrFromBytes(ipBytes)
	if ipErr != nil {
		return nil, ipErr
	}
	reservation.IP = ip
	reservation.Port = int32(port)

	ownerProcess, ownerErr := resourceLeaseOwnerFromDB(ownerPID, ownerIdentityTime)
	if ownerErr != nil {
		return nil, fmt.Errorf("could not read port reservation owner: %w", ownerErr)
	}
	reservation.OwnerProcess = ownerProcess
	reservation.UpdatedAt = timeFromUnixNano(updatedAtUnixNano)
	return &reservation, nil
}

func insertPortReservation(ctx context.Context, conn *sql.Conn, request PortReservationRequest, now time.Time) error {
	_, insertErr := conn.ExecContext(
		ctx,
		`INSERT INTO port_allocations(
			protocol, ip, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 )
		 VALUES(?, ?, ?, ?, ?, ?)`,
		request.Protocol,
		portReservationIPBytes(request.IP),
		request.Port,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
		unixNano(now),
	)
	if insertErr != nil {
		return fmt.Errorf("could not insert port reservation '%s/%s/%d': %w", request.Protocol, request.IP, request.Port, insertErr)
	}
	return nil
}

func updatePortReservation(ctx context.Context, conn *sql.Conn, request PortReservationRequest, now time.Time) error {
	_, updateErr := conn.ExecContext(
		ctx,
		`UPDATE port_allocations
		 SET owner_pid = ?, owner_identity_time = ?, updated_at_unix_nano = ?
		 WHERE protocol = ? AND ip = ? AND port = ?`,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
		unixNano(now),
		request.Protocol,
		portReservationIPBytes(request.IP),
		request.Port,
	)
	if updateErr != nil {
		return fmt.Errorf("could not update port reservation '%s/%s/%d': %w", request.Protocol, request.IP, request.Port, updateErr)
	}
	return nil
}

func deletePortReservation(ctx context.Context, conn *sql.Conn, reservation *PortReservation) error {
	_, deleteErr := conn.ExecContext(
		ctx,
		`DELETE FROM port_allocations
		 WHERE protocol = ? AND ip = ? AND port = ?
			AND owner_pid = ? AND owner_identity_time = ?`,
		reservation.Protocol,
		portReservationIPBytes(reservation.IP),
		reservation.Port,
		reservation.OwnerProcess.Pid,
		timeString(reservation.OwnerProcess.IdentityTime),
	)
	if deleteErr != nil {
		return fmt.Errorf("could not delete port reservation '%s/%s/%d': %w", reservation.Protocol, reservation.IP, reservation.Port, deleteErr)
	}
	return nil
}

func deleteExactPortReservation(ctx context.Context, conn *sql.Conn, request PortReservationRequest) error {
	_, deleteErr := conn.ExecContext(
		ctx,
		`DELETE FROM port_allocations
		 WHERE protocol = ? AND ip = ? AND port = ?
			AND owner_pid = ? AND owner_identity_time = ?`,
		request.Protocol,
		portReservationIPBytes(request.IP),
		request.Port,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
	)
	if deleteErr != nil {
		return fmt.Errorf("could not delete port reservation '%s/%s/%d': %w", request.Protocol, request.IP, request.Port, deleteErr)
	}
	return nil
}

func (s *Store) inactivePortReservationCandidates(ctx context.Context) ([]PortReservation, error) {
	db, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}

	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT protocol, ip, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM port_allocations`,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list port reservations for cleanup: %w", queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	candidates := []PortReservation{}
	for rows.Next() {
		reservation, scanErr := scanPortReservation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read port reservation for cleanup: %w", scanErr)
		}
		if !resourceLeaseOwnerIsActive(reservation.OwnerProcess) {
			candidates = append(candidates, *reservation)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list port reservations for cleanup: %w", rowsErr)
	}

	return candidates, nil
}
