CREATE TABLE IF NOT EXISTS persistent_volumes (
	resource_key TEXT PRIMARY KEY,
	volume_name TEXT NOT NULL,
	runtime_name TEXT NOT NULL DEFAULT '',
	workload_id TEXT NOT NULL DEFAULT '',
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_persistent_volumes_workload_id ON persistent_volumes(workload_id);
