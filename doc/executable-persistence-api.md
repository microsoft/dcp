# Executable persistence

Executable resources can be configured so their underlying OS process survives a DCP controller restart and can be adopted by the next controller instance.

## `ExecutableSpec` fields

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
- Persistent Executables are supported only with `executionType: Process`, or with `executionType` omitted because `Process` is the default.
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

The schema includes:

- `persistent_processes`: process records used to adopt persistent Executables.
- `resource_locks`: lease records used for coordination between DCP processes.

## Go behavior

The following behavior is relevant to code that constructs Executable objects, implements Executable runners, or creates controller reconcilers in tests.

### Executable construction and comparison

Code that constructs Executable specs can opt into three behaviors: deferring initial start, making the run persistent, and controlling the lifecycle key used for reuse. Spec comparisons use effective start behavior, so omitted `start` and explicit `true` are treated as equivalent.

When callers omit `lifecycleKey`, the controller computes one from launch-relevant inputs. That computation can depend on resolved arguments, explicitly configured environment variables, environment files, and certificate configuration, so tests that exercise default lifecycle-key behavior need those referenced inputs to be available.

### Runner responsibilities

Executable runners distinguish between ordinary controller-owned runs and persistent runs. For ordinary runs, cancellation of the controller lifetime should still clean up the process. For persistent runs, the runner must let the process outlive the controller instance and must avoid any monitor or process flags that would kill the process when DCP exits.

Runners that support persistence also need an adoption path. Adoption reattaches DCP bookkeeping to a process started by an earlier controller instance rather than launching a new process. Implementations should validate the stored run metadata, verify the process by both PID and process identity time to protect against PID reuse, and reconnect the run to the normal completion/stop notification flow.

Runner results distinguish between the process identity timestamp and the display start timestamp. The identity timestamp is for correctness checks and persistence; the display timestamp is for user-facing status. Persistent runs must provide enough process identity information for the controller to safely adopt or reject a stored record later.

### Reconciler and state-store behavior

The Executable reconciler can be configured with a specific state store, which is useful for tests and hosts that need isolation. If no explicit store is provided, persistence code opens the default local SQLite store when needed.

Persistent Executable records are keyed by the Executable's namespaced name because the persistent-process table already scopes the record type. Runtime resource leases for containers and networks use a separate lease-key interface because those leases coordinate external Docker/Podman resources in a shared table; those keys are based on runtime resource names rather than Kubernetes object names.

The state store owns the details of persisting process identity, lifecycle metadata, and resource lease ownership. Callers should treat stored records as controller coordination data, not as a stable public serialization contract.

## Configuration checklist

When configuring persistent Executables:

1. Add `spec.persistent: true` only for Executables that should keep running when DCP exits or restarts.
2. Keep persistent Executables on `executionType: Process`; do not use IDE execution or fallback execution types with persistence.
3. Decide whether the default lifecycle-key hash is appropriate. If stable caller-controlled reuse is needed, set `spec.lifecycleKey`.
4. Do not update an existing Executable from non-persistent to persistent, or from persistent to non-persistent. Recreate the object instead.
5. Do not set `spec.start: false` on an Executable that was already created with start enabled. Recreate the object if a not-yet-started resource is required.
6. Use `spec.stop: true` when the persistent process should be stopped; deleting the Executable object alone leaves the process running.
7. If tests need isolation from a developer's real state store, set `DCP_STATE_STORE_PATH` to a temporary SQLite file.
