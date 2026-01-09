// Copyright (c) Microsoft Corporation. All rights reserved.

package networking

import (
	"cmp"
	"os"
	"path/filepath"
	std_slices "slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

func TestMruFileReadWrite(t *testing.T) {
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), t.Name()+".list")
	defer func() {
		_ = os.Remove(path) // Best effort cleanup
	}()

	params := defaultMruPortFileUsageParameters()
	params.failOnPortFileError = true
	pf, err := newMruPortFile(path, params)
	require.NoError(t, err)

	usedPorts, readErr := pf.tryLockAndRead(testCtx)
	require.NoError(t, readErr)
	require.Empty(t, usedPorts)

	for i := 0; i < 10; i++ {
		var address string
		if i%2 == 0 {
			address = "127.0.0.1"
		} else {
			address = "127.0.0.2"
		}
		port := int32(10000 + i*10)
		r := mruPortFileRecord{
			Address:        address,
			Port:           port,
			AddressAndPort: AddressAndPort(address, port),
			Timestamp:      time.Now(),
			Instance:       GetProgramInstanceID(),
		}
		usedPorts = append(usedPorts, r)
	}

	writeErr := pf.WriteAndUnlock(testCtx, usedPorts)
	require.NoError(t, writeErr)

	usedPorts, readErr = pf.tryLockAndRead(testCtx)
	require.NoError(t, readErr)
	require.Len(t, usedPorts, 10)

	// Ports should be sorted by (address, port) so we can use binary search over the slice
	// The content arter sorting will be
	// (127.0.0.1, 10000), (127.0.0.1, 10020), ... (127.0.0.1, 10080),
	// (127.0.0.2, 10010), (127.0.0.2, 10030), ... (127.0.0.2, 10090).
	previous := ""
	for _, r := range usedPorts {
		require.Equal(t, 1, cmp.Compare(r.AddressAndPort, previous))
		previous = r.AddressAndPort
	}

	// Should be able to find "first", "last", and "somewhere in the middle" records
	i, found := std_slices.BinarySearchFunc(usedPorts, AddressAndPort("127.0.0.1", 10000), matchAddressAndPort)
	require.True(t, found)
	require.Equal(t, 0, i)
	i, found = std_slices.BinarySearchFunc(usedPorts, AddressAndPort("127.0.0.2", 10090), matchAddressAndPort)
	require.True(t, found)
	require.Equal(t, 9, i)
	i, found = std_slices.BinarySearchFunc(usedPorts, AddressAndPort("127.0.0.1", 10060), matchAddressAndPort)
	require.True(t, found)
	require.Equal(t, 3, i)

	closeErr := pf.Close()
	require.NoError(t, closeErr)
}

func TestOldPortEntriesRemoved(t *testing.T) {
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), t.Name()+".list")
	defer func() {
		_ = os.Remove(path) // Best effort cleanup
	}()

	params := defaultMruPortFileUsageParameters()
	params.failOnPortFileError = true
	params.recentPortLifetime = 2 * time.Second
	pf, err := newMruPortFile(path, params)
	require.NoError(t, err)

	usedPorts, readErr := pf.tryLockAndRead(testCtx)
	require.NoError(t, readErr)
	require.Empty(t, usedPorts)

	for i := 0; i < 10; i++ {
		port := int32(10000 + i*10)

		var timestamp time.Time
		if i%2 == 0 {
			timestamp = time.Now()
		} else {
			timestamp = time.Now().Add(-3 * time.Second)
		}

		r := mruPortFileRecord{
			Address:        "127.0.0.1",
			Port:           port,
			AddressAndPort: AddressAndPort("127.0.0.1", port),
			Timestamp:      timestamp,
			Instance:       GetProgramInstanceID(),
		}
		usedPorts = append(usedPorts, r)
	}

	writeErr := pf.WriteAndUnlock(testCtx, usedPorts)
	require.NoError(t, writeErr)

	usedPorts, readErr = pf.tryLockAndRead(testCtx)
	require.NoError(t, readErr)
	require.Len(t, usedPorts, 5, "Expect only 5 records to remain after removing old entries")
	for _, r := range usedPorts {
		require.True(t, r.Timestamp.After(time.Now().Add(-1*params.recentPortLifetime)))
	}

	closeErr := pf.Close()
	require.NoError(t, closeErr)
}
