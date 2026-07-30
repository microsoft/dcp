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

### Serializing concurrent migrations

`golang-migrate` does not serialize migrations across processes when using SQLite. Its `Driver` interface lets an implementation opt out of locking, and the SQLite driver does: `Lock` only flips an in-process flag on the driver instance. DCP builds a fresh migration runner for every `Open`, so that flag serializes nothing at all.

The lock DCP takes in `migrate` is therefore the only thing that keeps two DCP instances from migrating the same store at once, and it must stay wrapped around the whole read, repair, and migrate sequence. `golang-migrate` reads the current version and then writes the version marker in separate transactions, so instances that are not serialized can both read the same version, both decide to migrate, and both run the same migration — the loser fails to start or leaves a dirty marker behind.

The lock is an advisory file lock held on an open file descriptor, so the operating system releases it when the owning process exits, including a crash. DCP waits for the lock rather than failing when another instance holds it, which also covers Windows releasing the lock asynchronously after a process dies. An interrupted migration therefore leaves a dirty version marker but never a permanently stuck lock, which is what lets the next DCP acquire the lock and repair the marker.

### Recovering a dirty version

`golang-migrate` marks a version as dirty before running its SQL and clears the dirty flag after the SQL succeeds. A dirty version therefore means a migration did not complete. That happens when DCP is interrupted part way through a migration, and also when an older DCP re-runs a migration a newer DCP already applied: the older binary expects a version marker the newer layout no longer records, re-runs the migration, and its SQL fails because the schema change is already present.

DCP repairs a dirty version instead of failing startup. Every migration registers an *applied probe* in `schema.go`: a SQL predicate that reports whether that migration's schema changes are present. DCP evaluates the probe for the dirty version and rewrites the version table:

- The probe reports true, so the migration committed. DCP records the dirty version as clean and continues with the following migration.
- The probe reports false, so the migration never committed. DCP records the preceding version as clean, or empties the stream when the dirty version was the first migration, and the migration runs again.

This is sound because the migration driver wraps each migration in its own transaction. An interrupted migration either committed in full or never committed at all, so the schema always matches one of the two cases above and is never partially migrated. DCP repairs the major version stream and each major version's minor stream the same way.

This repair depends on two invariants, both covered by unit tests:

- Every migration has an applied probe, and that probe reports false before the migration runs and true afterwards. Without a probe, DCP cannot tell whether the migration committed and startup fails with the dirty version. A probe that does not distinguish the two states is worse: DCP would keep a version marker for a migration that never ran, permanently skipping it.
- Migration SQL never commits implicitly. `BEGIN`, `COMMIT`, `END`, `ROLLBACK`, `VACUUM`, `PRAGMA`, `ATTACH`, and `DETACH` would break migration atomicity and are rejected. SQLite treats `END` as an alias for `COMMIT`; an `END` that closes a `CASE` expression is fine.

DCP does not repair every dirty marker:

- A dirty minor version newer than any migration embedded in this binary cannot be probed, because the migration belongs to a newer DCP. Minor migrations are additive, so every migration this binary needs is present regardless of whether the interrupted one committed; DCP logs the condition, leaves the marker alone, and lets the binary that owns the migration repair it.
- An unknown major version fails startup whether it is dirty or clean. Major versions are reserved for changes that older binaries cannot use safely.

### Accepting a newer clean minor version

An older DCP may find a clean minor version newer than any migration embedded in its binary. DCP accepts that version without invoking `golang-migrate`. Calling the library with an unknown current version would fail because the older binary does not contain that migration file. Accepting the version is safe only when every minor migration follows the compatibility rules below.

## Schema version 2 transition

The original migration layout used one sequence for every schema change: version 1 created the state store, and version 2 added workload IDs and persistent container and network tables. Version 2 was additive and remained compatible with DCP binaries that only knew version 1. However, recording version 2 in `schema_migrations` caused those older binaries to fail.

The new layout classifies that change as major version 1, minor version 1. When DCP finds a clean database created by the old version 2 migration, it records minor version 1 in `schema_minor_migrations_v1` and changes the major marker from 2 back to 1. This only reclassifies an already-applied migration; it does not undo schema changes or delete data.

Major version 2 remains reserved for this transition. The next breaking schema change must use major version 3.

## Adding a compatible minor migration

Add compatible migrations beneath the major migration they extend. For example, migrations for major version 1 belong in `000001_initial/`. Use normal `golang-migrate` file names such as `000002_description.up.sql`, update that major version's `latestMinorVersion` in `schema.go`, and add an applied probe for the new version to that major version's `minorProbes`.

The applied probe must return a single boolean column that is true exactly when the migration's schema changes are present, and it must remain valid against both the pre-migration and post-migration schema. Probing `sqlite_master` for a new table or `pragma_table_info` for a new column works well.

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
- Do not include `BEGIN`, `COMMIT`, `END`, `ROLLBACK`, `VACUUM`, `PRAGMA`, `ATTACH`, or `DETACH`; the SQLite migration driver wraps each migration in a transaction and these statements would break its atomicity. SQLite treats `END` as an alias for `COMMIT`, so only an `END` that closes a `CASE` expression is allowed.

## Adding a breaking major migration

Use a new major version only when the resulting schema cannot remain safe for older DCP binaries.

Add its migration at the migration root, register it in `schemaMajorMigrations`, add an applied probe for the new version to `schemaMajorProbes`, and give it a dedicated minor migration table. Compatible follow-up migrations belong in a directory named after the major migration. Once the major marker advances, older DCP binaries will reject the database.
