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

	"github.com/microsoft/dcp/pkg/process"
)

const (
	ipv4AddressBytes = 4
	ipv6AddressBytes = 16
)

var ErrPortReservationHeld = errors.New("port reservation is held by another owner")

type PortReservation struct {
	Protocol     string
	Address      string
	addressBytes []byte
	Port         int32
	OwnerProcess process.ProcessTreeItem
	UpdatedAt    time.Time
}

type PortReservationRequest struct {
	Protocol     string
	Address      string
	addressBytes []byte
	Port         int32
	OwnerProcess process.ProcessTreeItem
}

type PortReservationHeldError struct {
	Reservation PortReservation
}

func (e *PortReservationHeldError) Error() string {
	return fmt.Sprintf("%s: %s/%s/%d", ErrPortReservationHeld, e.Reservation.Protocol, e.Reservation.Address, e.Reservation.Port)
}

func (e *PortReservationHeldError) Unwrap() error {
	return ErrPortReservationHeld
}

func HeldPortReservation(err error) (*PortReservation, bool) {
	var heldErr *PortReservationHeldError
	if !errors.As(err, &heldErr) {
		return nil, false
	}

	reservation := heldErr.Reservation
	return &reservation, true
}

func (s *Store) ReservePort(ctx context.Context, request PortReservationRequest) (*PortReservation, error) {
	return s.reservePort(ctx, request, false)
}

func (s *Store) ReserveSpecificPort(ctx context.Context, request PortReservationRequest) (*PortReservation, error) {
	return s.reservePort(ctx, request, true)
}

func (s *Store) IsPortReservedByOtherOwner(ctx context.Context, protocol string, address string, port int32, ownerProcess process.ProcessTreeItem) (bool, error) {
	key, portErr := normalizePortReservationKey(protocol, address, port)
	if portErr != nil {
		return false, portErr
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(ownerProcess)
	if ownerErr != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}

	db, dbErr := s.requireDB()
	if dbErr != nil {
		return false, dbErr
	}

	reservations, reservationsErr := getConflictingPortReservations(ctx, db, key)
	if reservationsErr != nil {
		return false, reservationsErr
	}
	for _, reservation := range reservations {
		if reservation.OwnerProcess.Pid == normalizedOwner.Pid &&
			reservation.OwnerProcess.IdentityTime.Equal(normalizedOwner.IdentityTime) {
			continue
		}
		if resourceLeaseOwnerIsActive(reservation.OwnerProcess) {
			return true, nil
		}
	}

	return false, nil
}

func (s *Store) ReleasePort(ctx context.Context, request PortReservationRequest) error {
	normalizedRequest, requestErr := normalizePortReservationRequest(request)
	if requestErr != nil {
		return requestErr
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		return deleteExactPortReservation(ctx, conn, normalizedRequest)
	})
}

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
				 WHERE protocol = ? AND address = ? AND port = ?
					AND owner_pid = ? AND owner_identity_time = ?
					AND updated_at_unix_nano = ?`,
				candidate.Protocol,
				candidate.addressBytes,
				candidate.Port,
				candidate.OwnerProcess.Pid,
				timeString(candidate.OwnerProcess.IdentityTime),
				unixNano(candidate.UpdatedAt),
			)
			if execErr != nil {
				return fmt.Errorf("could not delete inactive port reservation '%s/%s/%d': %w", candidate.Protocol, candidate.Address, candidate.Port, execErr)
			}
		}
		return nil
	})
}

func (s *Store) reservePort(ctx context.Context, request PortReservationRequest, reuseSameOwner bool) (*PortReservation, error) {
	normalizedRequest, requestErr := normalizePortReservationRequest(request)
	if requestErr != nil {
		return nil, requestErr
	}

	now := time.Now().UTC()
	var reservation *PortReservation
	txErr := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		requestKey := normalizedPortReservationKey{
			protocol:     normalizedRequest.Protocol,
			address:      normalizedRequest.Address,
			addressBytes: normalizedRequest.addressBytes,
			port:         normalizedRequest.Port,
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
			reservation, _, readErr = getExactPortReservation(ctx, conn, normalizedRequest.Protocol, normalizedRequest.Address, normalizedRequest.Port)
			return readErr
		}

		inactiveReservations := make([]*PortReservation, 0, len(conflictingReservations))
		shouldUpdateExactReservation := false
		for _, conflictingReservation := range conflictingReservations {
			exactAddress := bytes.Equal(conflictingReservation.addressBytes, normalizedRequest.addressBytes)
			sameOwner := conflictingReservation.OwnerProcess.Pid == normalizedRequest.OwnerProcess.Pid &&
				conflictingReservation.OwnerProcess.IdentityTime.Equal(normalizedRequest.OwnerProcess.IdentityTime)
			if reuseSameOwner && sameOwner {
				if exactAddress {
					shouldUpdateExactReservation = true
					continue
				}
			}
			active := resourceLeaseOwnerIsActive(conflictingReservation.OwnerProcess)
			if !active {
				if exactAddress {
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
		reservation, _, readErr = getExactPortReservation(ctx, conn, normalizedRequest.Protocol, normalizedRequest.Address, normalizedRequest.Port)
		return readErr
	})
	if txErr != nil {
		return nil, txErr
	}

	return reservation, nil
}

func normalizePortReservationRequest(request PortReservationRequest) (PortReservationRequest, error) {
	key, keyErr := normalizePortReservationKey(request.Protocol, request.Address, request.Port)
	if keyErr != nil {
		return PortReservationRequest{}, keyErr
	}
	normalizedOwner, ownerErr := normalizeResourceLeaseOwner(request.OwnerProcess)
	if ownerErr != nil {
		return PortReservationRequest{}, fmt.Errorf("%w: %w", ErrInvalidArgument, ownerErr)
	}

	return PortReservationRequest{
		Protocol:     key.protocol,
		Address:      key.address,
		addressBytes: key.addressBytes,
		Port:         key.port,
		OwnerProcess: normalizedOwner,
	}, nil
}

type normalizedPortReservationKey struct {
	protocol     string
	address      string
	addressBytes []byte
	port         int32
}

func normalizePortReservationKey(protocol string, address string, port int32) (normalizedPortReservationKey, error) {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	if protocol == "" {
		return normalizedPortReservationKey{}, fmt.Errorf("%w: port reservation protocol cannot be empty", ErrInvalidArgument)
	}
	addr, addressErr := parsePortReservationAddress(address)
	if addressErr != nil {
		return normalizedPortReservationKey{}, addressErr
	}
	if port < 1 || port > 65535 {
		return normalizedPortReservationKey{}, fmt.Errorf("%w: port reservation port must be between 1 and 65535", ErrInvalidArgument)
	}
	addr = addr.Unmap()
	return normalizedPortReservationKey{
		protocol:     protocol,
		address:      addr.String(),
		addressBytes: portReservationAddressBytes(addr),
		port:         port,
	}, nil
}

func parsePortReservationAddress(address string) (netip.Addr, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return netip.Addr{}, fmt.Errorf("%w: port reservation address cannot be empty", ErrInvalidArgument)
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		address = strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
	}
	addr, parseErr := netip.ParseAddr(address)
	if parseErr != nil {
		return netip.Addr{}, fmt.Errorf("%w: port reservation address must be an IP address: %w", ErrInvalidArgument, parseErr)
	}
	return addr, nil
}

func portReservationAddressBytes(addr netip.Addr) []byte {
	if addr.Is4() {
		bytes := addr.As4()
		return append([]byte(nil), bytes[:]...)
	}
	bytes := addr.As16()
	return append([]byte(nil), bytes[:]...)
}

func portReservationAddressFromBytes(addressBytes []byte) (string, error) {
	addr, addrErr := portReservationAddrFromBytes(addressBytes)
	if addrErr != nil {
		return "", addrErr
	}
	return addr.String(), nil
}

func portReservationAddrFromBytes(addressBytes []byte) (netip.Addr, error) {
	switch len(addressBytes) {
	case ipv4AddressBytes:
		var bytes [ipv4AddressBytes]byte
		copy(bytes[:], addressBytes)
		return netip.AddrFrom4(bytes), nil
	case ipv6AddressBytes:
		var bytes [ipv6AddressBytes]byte
		copy(bytes[:], addressBytes)
		return netip.AddrFrom16(bytes), nil
	default:
		return netip.Addr{}, fmt.Errorf("port reservation address has invalid byte length %d", len(addressBytes))
	}
}

type portReservationScanner interface {
	Scan(dest ...any) error
}

type portReservationQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getExactPortReservation(ctx context.Context, querier portReservationQuerier, protocol string, address string, port int32) (*PortReservation, bool, error) {
	key, keyErr := normalizePortReservationKey(protocol, address, port)
	if keyErr != nil {
		return nil, false, keyErr
	}
	row := querier.QueryRowContext(
		ctx,
		`SELECT protocol, address, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM port_allocations
		 WHERE protocol = ? AND address = ? AND port = ?`,
		key.protocol,
		key.addressBytes,
		key.port,
	)

	reservation, scanErr := scanPortReservation(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, false, nil
	}
	if scanErr != nil {
		return nil, false, fmt.Errorf("could not read port reservation '%s/%s/%d': %w", key.protocol, key.address, key.port, scanErr)
	}
	return reservation, true, nil
}

func getConflictingPortReservations(ctx context.Context, querier portReservationQuerier, key normalizedPortReservationKey) ([]*PortReservation, error) {
	rows, queryErr := querier.QueryContext(
		ctx,
		`SELECT protocol, address, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 FROM port_allocations
		 WHERE protocol = ? AND port = ?`,
		key.protocol,
		key.port,
	)
	if queryErr != nil {
		return nil, fmt.Errorf("could not list port reservations for '%s/%s/%d': %w", key.protocol, key.address, key.port, queryErr)
	}
	defer func() {
		_ = rows.Close()
	}()

	reservations := []*PortReservation{}
	for rows.Next() {
		reservation, scanErr := scanPortReservation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read port reservation for '%s/%s/%d': %w", key.protocol, key.address, key.port, scanErr)
		}
		if portReservationAddressesConflict(key.addressBytes, reservation.addressBytes) {
			reservations = append(reservations, reservation)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("could not list port reservations for '%s/%s/%d': %w", key.protocol, key.address, key.port, rowsErr)
	}

	return reservations, nil
}

func portReservationAddressesConflict(requestedAddress []byte, reservedAddress []byte) bool {
	if bytes.Equal(requestedAddress, reservedAddress) {
		return true
	}
	if len(requestedAddress) != len(reservedAddress) {
		return false
	}

	requestedAddr, requestedErr := portReservationAddrFromBytes(requestedAddress)
	if requestedErr != nil {
		return false
	}
	reservedAddr, reservedErr := portReservationAddrFromBytes(reservedAddress)
	if reservedErr != nil {
		return false
	}

	return requestedAddr.IsUnspecified() || reservedAddr.IsUnspecified()
}

func scanPortReservation(row portReservationScanner) (*PortReservation, error) {
	var reservation PortReservation
	var addressBytes []byte
	var port int64
	var ownerPID int64
	var ownerIdentityTime string
	var updatedAtUnixNano int64

	scanErr := row.Scan(
		&reservation.Protocol,
		&addressBytes,
		&port,
		&ownerPID,
		&ownerIdentityTime,
		&updatedAtUnixNano,
	)
	if scanErr != nil {
		return nil, scanErr
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port reservation port is out of range: %d", port)
	}
	address, addressErr := portReservationAddressFromBytes(addressBytes)
	if addressErr != nil {
		return nil, addressErr
	}
	reservation.Address = address
	reservation.addressBytes = append([]byte(nil), addressBytes...)
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
			protocol, address, port,
			owner_pid, owner_identity_time, updated_at_unix_nano
		 )
		 VALUES(?, ?, ?, ?, ?, ?)`,
		request.Protocol,
		request.addressBytes,
		request.Port,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
		unixNano(now),
	)
	if insertErr != nil {
		return fmt.Errorf("could not insert port reservation '%s/%s/%d': %w", request.Protocol, request.Address, request.Port, insertErr)
	}
	return nil
}

func updatePortReservation(ctx context.Context, conn *sql.Conn, request PortReservationRequest, now time.Time) error {
	_, updateErr := conn.ExecContext(
		ctx,
		`UPDATE port_allocations
		 SET owner_pid = ?, owner_identity_time = ?, updated_at_unix_nano = ?
		 WHERE protocol = ? AND address = ? AND port = ?`,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
		unixNano(now),
		request.Protocol,
		request.addressBytes,
		request.Port,
	)
	if updateErr != nil {
		return fmt.Errorf("could not update port reservation '%s/%s/%d': %w", request.Protocol, request.Address, request.Port, updateErr)
	}
	return nil
}

func deletePortReservation(ctx context.Context, conn *sql.Conn, reservation *PortReservation) error {
	_, deleteErr := conn.ExecContext(
		ctx,
		`DELETE FROM port_allocations
		 WHERE protocol = ? AND address = ? AND port = ?
			AND owner_pid = ? AND owner_identity_time = ?`,
		reservation.Protocol,
		reservation.addressBytes,
		reservation.Port,
		reservation.OwnerProcess.Pid,
		timeString(reservation.OwnerProcess.IdentityTime),
	)
	if deleteErr != nil {
		return fmt.Errorf("could not delete port reservation '%s/%s/%d': %w", reservation.Protocol, reservation.Address, reservation.Port, deleteErr)
	}
	return nil
}

func deleteExactPortReservation(ctx context.Context, conn *sql.Conn, request PortReservationRequest) error {
	_, deleteErr := conn.ExecContext(
		ctx,
		`DELETE FROM port_allocations
		 WHERE protocol = ? AND address = ? AND port = ?
			AND owner_pid = ? AND owner_identity_time = ?`,
		request.Protocol,
		request.addressBytes,
		request.Port,
		request.OwnerProcess.Pid,
		timeString(request.OwnerProcess.IdentityTime),
	)
	if deleteErr != nil {
		return fmt.Errorf("could not delete port reservation '%s/%s/%d': %w", request.Protocol, request.Address, request.Port, deleteErr)
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
		`SELECT protocol, address, port,
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
