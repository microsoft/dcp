# State store migration authoring rules

DCP applies these SQLite migrations with `golang-migrate/migrate` using the pure-Go `modernc.org/sqlite` driver. Migration files use golang-migrate names such as `000001_initial.up.sql`. DCP only applies `up` migrations at runtime; do not add destructive `down` migrations for the local state store.

Migration SQL files are embedded into the DCP binary from `schema.go` with `go:embed`, so runtime schema initialization does not read migrations from the filesystem.

The local state store is shared by multiple DCP processes and may be opened by older DCP binaries after a newer binary has run. Do not treat the migration version as a global compatibility gate. Runtime code that needs a newer schema shape should probe the specific table, column, or capability it needs and fail only that feature with an actionable error.

## Compatibility rules

- Prefer expand/migrate/contract. Add compatible schema first, teach code to dual-read or dual-write where needed, and only remove old schema after the supported compatibility window has intentionally ended.
- Application SQL must use explicit column lists for inserts and selects so added columns do not break older binaries.
- Allowed by default: `CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`, and `ALTER TABLE ... ADD COLUMN` when the column is nullable or has a safe constant `DEFAULT`.
- Allowed with care: non-unique indexes, idempotent backfills that preserve existing column meaning, and additive columns that newer code can ignore when absent.
- Requires explicit compatibility design: unique indexes, check constraints, foreign keys, changed primary keys, table rebuilds, or any constraint that can reject writes older DCP binaries still perform.
- Do not add a `NOT NULL` column without a default to an existing table.
- Do not rename or drop tables/columns, change column meaning, or make older binaries blind to locks/process records during the compatibility window.
- Treat `resource_locks` as ephemeral but still compatibility-sensitive: old and new DCP instances must coordinate through the same lock representation until old writers are no longer supported.
- Treat `persistent_processes` as durable metadata. Migrations must preserve existing records unless an explicit product decision retires the stored state.
- Do not include `BEGIN` or `COMMIT` statements. The golang-migrate SQLite driver wraps each migration in a transaction.
