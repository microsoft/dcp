CREATE TABLE IF NOT EXISTS port_allocations (
	protocol TEXT NOT NULL,
	address BLOB NOT NULL CHECK(length(address) IN (4, 16)),
	port INTEGER NOT NULL,
	owner_pid INTEGER NOT NULL,
	owner_identity_time TEXT NOT NULL,
	updated_at_unix_nano INTEGER NOT NULL,
	PRIMARY KEY(protocol, address, port)
);

CREATE INDEX IF NOT EXISTS idx_port_allocations_owner ON port_allocations(owner_pid, owner_identity_time);
