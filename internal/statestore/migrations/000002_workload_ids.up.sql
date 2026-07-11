ALTER TABLE persistent_processes ADD COLUMN workload_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_persistent_processes_workload_id ON persistent_processes(workload_id);

CREATE TABLE IF NOT EXISTS persistent_containers (
	resource_key TEXT PRIMARY KEY,
	container_id TEXT NOT NULL,
	container_name TEXT NOT NULL DEFAULT '',
	workload_id TEXT NOT NULL DEFAULT '',
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_persistent_containers_workload_id ON persistent_containers(workload_id);

CREATE TABLE IF NOT EXISTS persistent_networks (
	resource_key TEXT PRIMARY KEY,
	network_id TEXT NOT NULL,
	network_name TEXT NOT NULL DEFAULT '',
	workload_id TEXT NOT NULL DEFAULT '',
	updated_at_unix_nano INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_persistent_networks_workload_id ON persistent_networks(workload_id);
