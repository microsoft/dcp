/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpdns

import (
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Records serves name -> IPv4 lookups backed by a hosts-format file
// ("<ip> <name> [<name> ...]" per line, '#' comments). The file is re-read when its
// modification time changes, so the publisher (DCP) can update records by atomically
// rewriting the file — no control channel between DCP and the forwarder is needed.
type Records struct {
	path string

	mu        sync.Mutex
	loadedAt  time.Time // mod time of the file content currently loaded
	checkedAt time.Time // when the file was last (re)read
	byName    map[string][]netip.Addr
}

// How long loaded records may be served before the file is unconditionally re-read.
// The records file is updated on the host and observed through a shared filesystem
// (virtiofs) whose attribute caching can delay modification-time changes, so a pure
// mod-time check could serve stale records indefinitely.
const recordsMaxAge = 1 * time.Second

func NewRecords(path string) *Records {
	return &Records{path: path}
}

// Lookup returns the IPv4 addresses for the given name (case-insensitive, with or
// without a trailing dot), or false if the name is not known.
func (r *Records) Lookup(name string) ([]netip.Addr, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))

	r.mu.Lock()
	defer r.mu.Unlock()

	r.refreshLocked()

	addrs, found := r.byName[name]
	return addrs, found
}

func (r *Records) refreshLocked() {
	info, statErr := os.Stat(r.path)
	if statErr != nil {
		// Treat a missing or unreadable file as "no records"; it may reappear later.
		r.byName = nil
		r.loadedAt = time.Time{}
		return
	}

	if r.byName != nil && !info.ModTime().After(r.loadedAt) && time.Since(r.checkedAt) < recordsMaxAge {
		return // Cached content is fresh enough
	}

	content, readErr := os.ReadFile(r.path)
	if readErr != nil {
		r.byName = nil
		r.loadedAt = time.Time{}
		return
	}
	r.checkedAt = time.Now()

	byName := map[string][]netip.Addr{}
	for _, line := range strings.Split(string(content), "\n") {
		if commentStart := strings.IndexByte(line, '#'); commentStart >= 0 {
			line = line[:commentStart]
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		addr, parseErr := netip.ParseAddr(fields[0])
		if parseErr != nil || !addr.Is4() {
			continue // Only IPv4 records are served
		}

		for _, name := range fields[1:] {
			name = strings.ToLower(strings.TrimSuffix(name, "."))
			byName[name] = append(byName[name], addr)
		}
	}

	r.byName = byName
	r.loadedAt = info.ModTime()
}
