# State store migration authoring rules

DCP uses separate `golang-migrate` streams for compatibility boundaries and compatible schema evolution:

- `schema_migrations` records the schema **major** version. Root-level migration files advance this marker only for deliberately incompatible changes.
- Each schema major has a minor migration table managed by `golang-migrate`. Major 1 uses `schema_minor_migrations_v1`, with its compatible migration files nested under `000001_initial/`.

Migration files are embedded into the DCP binary from `schema.go`; runtime initialization does not read them from the filesystem.

## Compatibility contract

The state store is shared by multiple DCP processes and may be opened by older DCP binaries after a newer binary has run.

- Before applying minor migrations, DCP reads the minor table's current version.
- A dirty minor version is an error.
- If the database minor version is newer than the latest migration embedded in the binary, DCP treats it as compatible and does not invoke `golang-migrate`.
- Otherwise, DCP delegates ordering, transactions, dirty tracking, and SQL execution to `golang-migrate`.
- Increment the schema major only for a deliberately incompatible schema change. Older DCP binaries are expected to reject an unknown major.

Schema major 1 is the currently shipped compatibility baseline. Schema version 2 was emitted by preview builds for the compatible `workload_ids` migration. Because minor streams have their own version namespace, current DCP imports that state as minor version 1 in `schema_minor_migrations_v1` and restores `schema_migrations` to major version 1. Version 3 is therefore the next available schema major for a future breaking change.

The forward-version check is required. Calling `Up` or `Migrate` when the database is already at a version absent from the binary's migration source causes `golang-migrate` to reject the unknown version.

## Compatible minor migrations

Nest compatible migration SQL under the major migration it extends. For example, compatible migrations for `000001_initial.up.sql` live under `000001_initial/`. Update that major's `latestMinorVersion` registration whenever a migration is added.

Minor migrations form a linear history: reaching a newer version means every earlier migration in that major's stream was successfully applied. Do not include `BEGIN` or `COMMIT` statements; the SQLite driver wraps each migration file in a transaction.

Prefer expand/migrate/contract:

- Add compatible schema before depending on it, dual-read or dual-write where necessary, and remove old schema only after intentionally ending the compatibility window.
- Use explicit column lists for application inserts and selects so added columns do not break older binaries.
- `CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`, and additive columns that are nullable or have a safe constant `DEFAULT` are compatible by default.
- Non-unique indexes and idempotent backfills that preserve existing column meaning are usually compatible.
- Unique indexes, check constraints, foreign keys, changed primary keys, and table rebuilds require an explicit compatibility design because they can reject writes from older binaries.
- Do not add a `NOT NULL` column without a default to an existing table.
- Do not rename or drop tables or columns, change column meaning, or make older binaries blind to locks or persistent records within a schema major.

Treat `resource_locks` as ephemeral but compatibility-sensitive: old and new DCP instances must coordinate through the same lock representation. Treat persistent resource tables as durable metadata and preserve their records unless an explicit product decision retires that state.

## Breaking major migrations

A breaking migration belongs at the migration root, must be added to `schemaMajorMigrations`, and must have an explicit upgrade path from the previous supported major. Its compatible follow-up migrations belong in a directory named after that major migration and use a separate minor migration table.

The migration sequence applies the stored major's known minor migrations before advancing to the next major, then applies that major's compatible minor stream. Advancing the major marker is an intentional backward-compatibility break.
