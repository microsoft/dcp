ALTER TABLE persistent_volumes
ADD COLUMN ownership_token TEXT NOT NULL DEFAULT '';
