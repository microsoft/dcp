ALTER TABLE persistent_containers ADD COLUMN runtime_name TEXT NOT NULL DEFAULT '';

ALTER TABLE persistent_networks ADD COLUMN runtime_name TEXT NOT NULL DEFAULT '';
