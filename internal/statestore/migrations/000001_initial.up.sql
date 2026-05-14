CREATE TABLE IF NOT EXISTS resource_locks (
	resource_key TEXT PRIMARY KEY,
	owner_pid INTEGER NOT NULL,
	owner_identity_time TEXT NOT NULL,
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS persistent_processes (
	resource_key TEXT PRIMARY KEY,
	lifecycle_key TEXT NOT NULL,
	pid INTEGER NOT NULL,
	identity_time TEXT NOT NULL,
	run_id TEXT NOT NULL,
	stdout_file TEXT NOT NULL,
	stderr_file TEXT NOT NULL,
	lifecycle_metadata TEXT NOT NULL DEFAULT '',
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_resource_locks_updated_at ON resource_locks(updated_at_unix_nano);
