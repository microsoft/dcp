# State store migrations

DCP uses [`golang-migrate`](https://github.com/golang-migrate/migrate) to apply embedded SQLite migrations and record which migrations have completed. The state store can be opened by an older DCP after a newer DCP has updated it, so migrations are divided into major and minor versions:

- `schema_migrations` records the major schema version. A new major version means the schema is intentionally incompatible with older DCP binaries.
- Each major version has a separate `golang-migrate` table for compatible minor migrations. Major version 1 uses `schema_minor_migrations_v1`.

Migration SQL is embedded into the DCP binary by `schema.go`. DCP only applies up migrations.

## How migrations run

At startup, DCP acquires the state store migration lock and reads `schema_migrations`.

For each supported major version, DCP:

1. Applies that major version's remaining minor migrations.
2. Advances `schema_migrations` when moving to the next major version.
3. Applies the new major version's minor migrations.

`golang-migrate` marks a version as dirty before running its SQL and clears the dirty flag after the SQL succeeds. A dirty version therefore means a migration did not complete. DCP fails startup for a dirty major or minor version rather than guessing which schema changes were applied.

An older DCP may find a clean minor version newer than any migration embedded in its binary. DCP accepts that version without invoking `golang-migrate`. Calling the library with an unknown current version would fail because the older binary does not contain that migration file. Accepting the version is safe only when every minor migration follows the compatibility rules below.

DCP fails startup when it encounters an unknown major version. Major versions are reserved for changes that older binaries cannot use safely.

## Schema version 2 transition

The original migration layout used one sequence for every schema change: version 1 created the state store, and version 2 added workload IDs and persistent container and network tables. Version 2 was additive and remained compatible with DCP binaries that only knew version 1. However, recording version 2 in `schema_migrations` caused those older binaries to fail.

The new layout classifies that change as major version 1, minor version 1. When DCP finds a clean database created by the old version 2 migration, it records minor version 1 in `schema_minor_migrations_v1` and changes the major marker from 2 back to 1. This only reclassifies an already-applied migration; it does not undo schema changes or delete data.

Major version 2 remains reserved for this transition. The next breaking schema change must use major version 3.

## Adding a compatible minor migration

Add compatible migrations beneath the major migration they extend. For example, migrations for major version 1 belong in `000001_initial/`. Use normal `golang-migrate` file names such as `000002_description.up.sql`, and update that major version's `latestMinorVersion` in `schema.go`.

Minor migrations must remain safe when an older DCP uses the database afterward:

- Prefer additive changes and expand/migrate/contract sequencing.
- Use explicit column lists in application `INSERT` and `SELECT` statements.
- New columns on existing tables must be nullable or have a safe constant default.
- `CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`, and non-unique indexes are compatible when they do not change existing reads or writes.
- Do not rename or drop tables or columns used by older binaries.
- Do not change the meaning of existing columns.
- Do not add unique indexes, check constraints, foreign keys, changed primary keys, or other constraints that can reject writes accepted by older binaries without an explicit compatibility design.
- Preserve `resource_locks` so old and new DCP processes coordinate through the same lock records.
- Preserve persistent resource tables and records so older binaries can continue reading and updating them.
- Do not include `BEGIN` or `COMMIT`; the SQLite migration driver wraps each migration in a transaction.

## Adding a breaking major migration

Use a new major version only when the resulting schema cannot remain safe for older DCP binaries.

Add its migration at the migration root, register it in `schemaMajorMigrations`, and give it a dedicated minor migration table. Compatible follow-up migrations belong in a directory named after the major migration. Once the major marker advances, older DCP binaries will reject the database.
