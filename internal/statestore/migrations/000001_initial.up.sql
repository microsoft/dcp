CREATE TABLE IF NOT EXISTS resource_locks (
	resource_key TEXT PRIMARY KEY,
	owner_pid INTEGER NOT NULL,
	owner_identity_time TEXT NOT NULL,
	lease_until_unix_nano INTEGER NOT NULL,
	updated_at_unix_nano INTEGER NOT NULL,
	metadata TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS persistent_processes (
	resource_key TEXT PRIMARY KEY,
	namespace TEXT NOT NULL,
	name TEXT NOT NULL,
	uid TEXT NOT NULL,
	lifecycle_key TEXT NOT NULL,
	pid INTEGER NOT NULL,
	identity_time TEXT NOT NULL,
	display_start_time_unix_nano INTEGER NOT NULL,
	run_id TEXT NOT NULL,
	stdout_file TEXT NOT NULL,
	stderr_file TEXT NOT NULL,
	execution_type TEXT NOT NULL,
	lifecycle_metadata TEXT NOT NULL DEFAULT '',
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_resource_locks_lease_until ON resource_locks(lease_until_unix_nano);
CREATE INDEX IF NOT EXISTS idx_persistent_processes_name ON persistent_processes(namespace, name);
