# Executable persistence API changes

This branch adds API fields that let callers create Executable resources whose underlying OS process can survive a DCP controller restart and be adopted by the next controller instance.

## New `ExecutableSpec` fields

### `spec.start`

`start` is an optional boolean that controls whether the controller should attempt to start the Executable.

```yaml
spec:
  start: true
```

Behavior:

- Omitted means `true`.
- `false` is only useful on initial creation. It creates the Executable object without starting a process.
- Once an Executable has been created with `start` omitted or set to `true`, it cannot be updated to `false`.
- `start` does not replace `stop`; use `spec.stop: true` to request that a started Executable be stopped.

Use this when a caller needs to define the Executable object before allowing DCP to start it.

### `spec.persistent`

`persistent` is an optional boolean that marks the Executable's process as persistent across DCP runs.

```yaml
spec:
  persistent: true
```

Behavior:

- Omitted means `false`; existing Executables keep their previous non-persistent behavior.
- Persistent Executables are currently supported only with `executionType: Process`, or with `executionType` omitted because `Process` is the default.
- Persistent Executables cannot specify `fallbackExecutionTypes`.
- `persistent` is immutable after creation.
- Deleting a persistent Executable object releases DCP's ownership/lease metadata but intentionally leaves the underlying process running.
- Setting `spec.stop: true` still requests that DCP stop the underlying process.

When a persistent Executable starts successfully, DCP stores process identity metadata in the local SQLite state store. A later DCP controller instance can use that record to reattach status, logs, endpoints, and health probe handling to the already-running process instead of launching a duplicate process.

### `spec.lifecycleKey`

`lifecycleKey` is an optional string that identifies whether an existing persistent process record is reusable for a newly reconciled Executable.

```yaml
spec:
  persistent: true
  lifecycleKey: my-service-dev-loop
```

Behavior:

- If set, DCP uses this value directly.
- If omitted, DCP computes a lifecycle key from the launch-relevant parts of the Executable spec:
  - `executablePath`
  - `workingDirectory`
  - `executionType`
  - `ambientEnvironment.behavior`
  - effective `args` after template substitution
  - effective values for environment variables explicitly listed in `env`
  - effective values for environment variables loaded from `envFiles`
  - `pemCertificates`
- Inherited/system environment variables are ignored unless they are also explicitly listed in `env` or loaded from `envFiles`.
- When DCP finds a stored persistent process record, it adopts the process only if the stored lifecycle key matches the current lifecycle key.
- If the key does not match, DCP logs the likely source of the change for default lifecycle keys, deletes the stale process record, and starts a new process.

For default lifecycle keys, the persistent process record also stores lifecycle diagnostic metadata. On mismatch, DCP logs argument, environment, and other change categories. Environment diagnostics identify changed variable names and store value hashes rather than raw environment values.

Use an explicit `lifecycleKey` when the caller wants to control process reuse across object recreation, or when the default hash would be too sensitive to inputs that should not force a restart.

## Example persistent Executable

```yaml
apiVersion: usvc.dev/v1
kind: Executable
metadata:
  name: my-service
spec:
  executablePath: /path/to/my-service
  workingDirectory: /path/to/repo
  args:
    - --urls
    - http://127.0.0.1:5000
  executionType: Process
  persistent: true
  lifecycleKey: my-service
```

With this configuration:

1. The first DCP controller instance starts `/path/to/my-service`.
2. DCP records the process PID, process identity time, run ID, log file paths, execution type, lifecycle key, and lifecycle diagnostic metadata in the local state store.
3. If the controller exits and a new one starts, it reads the stored record.
4. If the process is still running and the lifecycle key still matches, DCP adopts that process instead of starting another copy.
5. If the process is gone or the lifecycle key changed, DCP removes the stale record and starts a new process.

## State store behavior

The backing store is a local SQLite database used for DCP coordination metadata, including persistent process records and resource leases.

- The default path is under the DCP per-user directory.
- `DCP_STATE_STORE_PATH` overrides the SQLite database path.
- Elevated/admin DCP processes use a separate default database name from non-elevated processes.
- DCP uses WAL mode and a busy timeout to coordinate concurrent controller processes.
- Migration SQL is embedded into the DCP binary, so runtime schema initialization does not depend on external migration files.

The initial schema includes:

- `persistent_processes`: process records used to adopt persistent Executables.
- `resource_locks`: lease records used for coordination between DCP processes.

## Go type and interface changes

These changes are relevant to code that constructs Executable objects, implements Executable runners, or creates controller reconcilers in tests.

### `api/v1.ExecutableSpec`

`ExecutableSpec` now includes:

```go
Start        *bool  `json:"start,omitempty"`
Persistent   bool   `json:"persistent,omitempty"`
LifecycleKey string `json:"lifecycleKey,omitempty"`
```

The `Equal` implementation includes all three fields. Code that compares specs or creates expected specs in tests should include these fields when they are relevant.

`ExecutableSpec` also has:

```go
func (es *ExecutableSpec) GetLifecycleKey() (string, bool, error)
```

The return values are:

1. The lifecycle key to use.
2. `true` if the key was computed from the spec, or `false` if `spec.lifecycleKey` supplied it.
3. An error if computing the default key failed, most commonly because an `envFiles` entry could not be read.

### `api/v1.Executable`

`Executable` now has:

```go
func (e *Executable) ShouldStart() bool
```

This returns `true` when `spec.start` is omitted or set to `true`, and `false` only when `spec.start` is explicitly `false`.

### `controllers.ExecutableRunner`

The `ExecutableRunner.StartRun` contract now distinguishes persistent and non-persistent runs:

```go
// When the passed context is cancelled, the run should be automatically
// terminated unless the Executable is persistent.
StartRun(ctx context.Context, exe *apiv1.Executable, runChangeHandler RunChangeHandler, log logr.Logger) *ExecutableStartResult
```

Runner implementations must not tie a persistent process lifetime to the controller lifetime context. The process runner does this by using `context.WithoutCancel(ctx)`, avoiding kill-on-dispose process creation flags, and skipping the DCP monitor cleanup process when `exe.Spec.Persistent` is true.

### `controllers.PersistentExecutableRunner`

Runners that support persistent process adoption implement this new interface:

```go
type PersistentExecutableRunner interface {
    ExecutableRunner

    AdoptRun(
        ctx context.Context,
        run ExecutableRunAdoptionInfo,
        runChangeHandler RunChangeHandler,
        log logr.Logger,
    ) error
}
```

`ExecutableRunAdoptionInfo` contains the metadata needed to reattach to a process started by an earlier DCP controller:

```go
type ExecutableRunAdoptionInfo struct {
    RunID               RunID
    Pid                 process.Pid_t
    ProcessIdentityTime time.Time
    StdOutFile          string
    StdErrFile          string
    CommandInfo         string
}
```

An adoption implementation should:

1. Validate that the run ID, PID, and process identity time are present.
2. Verify the process still exists using both PID and identity time, not PID alone.
3. Register the run so future stop/completion notifications go through the supplied `RunChangeHandler`.

### `controllers.ExecutableStartResult` and `ExecutableRunInfo`

Both types now carry two process-time values:

```go
ProcessIdentityTime time.Time
DisplayStartTime    time.Time
```

Use `ProcessIdentityTime` for correctness checks that protect against PID reuse. Use `DisplayStartTime` for user-visible start timestamps and status.

Persistent process records require a valid PID and non-zero `ProcessIdentityTime`. If a runner returns `Running` for a persistent Executable without those values, the controller cannot safely persist/adopt the run.

### `controllers.ExecutableReconcilerConfig`

`NewExecutableReconciler` remains available for default construction. Tests or specialized hosts can use:

```go
func NewExecutableReconcilerWithConfig(
    lifetimeCtx context.Context,
    client ctrl_client.Client,
    noCacheClient ctrl_client.Reader,
    log logr.Logger,
    executableRunners map[apiv1.ExecutionType]ExecutableRunner,
    healthProbeSet *health.HealthProbeSet,
    config ExecutableReconcilerConfig,
) *ExecutableReconciler
```

The config currently contains:

```go
type ExecutableReconcilerConfig struct {
    StateStore *statestore.Store
}
```

Pass a test-specific `StateStore` when tests need deterministic persistent Executable behavior. If no store is supplied, the reconciler opens the default local state store when persistence requires it.

### `internal/statestore`

The new state store package exposes the internal records used by the controllers:

```go
type PersistentProcessRecord struct {
    ResourceKey      string
    Name             types.NamespacedName
    UID              types.UID
    LifecycleKey     string
    PID              process.Pid_t
    IdentityTime     time.Time
    DisplayStartTime time.Time
    RunID            string
    StdOutFile       string
    StdErrFile       string
    ExecutionType    string
    UpdatedAt        time.Time
}
```

Relevant methods:

```go
func Open(ctx context.Context, options Options) (*Store, error)
func DefaultPath() (string, error)
func (s *Store) UpsertPersistentProcess(ctx context.Context, record PersistentProcessRecord) error
func (s *Store) GetPersistentProcess(ctx context.Context, resourceKey string) (*PersistentProcessRecord, error)
func (s *Store) DeletePersistentProcess(ctx context.Context, resourceKey string) error
```

Use `statestore.ResourceKey(statestore.ResourceKindExecutable, name)` to produce the resource key used for persistent Executable records.

## Compatibility and validation checklist

When updating callers or generated manifests for this branch:

1. Add `spec.persistent: true` only for Executables that should keep running when DCP exits or restarts.
2. Keep persistent Executables on `executionType: Process`; do not use IDE execution or fallback execution types with persistence.
3. Decide whether the default lifecycle-key hash is appropriate. If stable caller-controlled reuse is needed, set `spec.lifecycleKey`.
4. Do not update an existing Executable from non-persistent to persistent, or from persistent to non-persistent. Recreate the object instead.
5. Do not set `spec.start: false` on an Executable that was already created with start enabled. Recreate the object if a not-yet-started resource is required.
6. Use `spec.stop: true` when the persistent process should be stopped; deleting the Executable object alone leaves the process running.
7. If tests need isolation from a developer's real state store, set `DCP_STATE_STORE_PATH` to a temporary SQLite file.
