# Targeted container-volume reset (V1)

Create a `ContainerVolumeReset` operation through the Kubernetes API:

```http
POST /apis/usvc-dev.developer.microsoft.com/v1/containervolumeresets
Content-Type: application/json

{
  "apiVersion": "usvc-dev.developer.microsoft.com/v1",
  "kind": "ContainerVolumeReset",
  "metadata": {"name": "my-container-reset-001"},
  "spec": {
    "containerName": "my-container",
    "containerUid": "expected-container-api-uid"
  }
}
```

The target is the Container API name and its current metadata UID, not the physical
container ID. The spec is immutable. A missing, deleting, or replaced target fails
before destructive work. Only session and persistent Container modes support reset.
The operation's metadata name and UID identify the attempt; watch or GET its status:

```json
{
  "state": "Succeeded",
  "finishTimestamp": "2026-07-01T12:00:00.000000Z",
  "containerRemoved": true,
  "volumes": ["my-data"],
  "volumeGenerations": [{
    "volumeName": "my-data",
    "previousVolumeName": "my-data",
    "requestedGeneration": 1,
    "requested": true,
    "selectedVolumeName": "my-data-dcp-1"
  }]
}
```

States are `Pending`, `Running`, `Succeeded`, and `Failed`. Only `Succeeded`
confirms fresh storage was selected for every named volume. It does **not** confirm
permanent deletion of old data, application resumption, or application readiness.
`Failed` includes a message; `containerRemoved`, `volumes`, and `volumeGenerations`
report partial progress. A selection entry reports the requested generation,
whether its update was acknowledged, and an observed selected physical name.
`requested=false` with an update error can mean an unknown submission outcome,
not proof the update was rejected. An accepted
request can finish independently after operation failure or cancellation.
Terminal outcomes never change, even if storage is repaired later. Selection across
multiple volumes is not transactional; do not interpret failure as no changes.

The operation controller owns status and cancellation. The Container controller
serializes target stop/removal with ordinary lifecycle reconciliation, then requests
new generations. Only the Volume controller creates, selects, and retires storage.
Another active operation for the same target is refused rather than queued for
an implicit second reset.

DCP checks Container specifications and all runtime containers, including stopped
containers retaining mounts, before stopping or removing the target:

```json
{
  "state": "Failed",
  "message": "named volumes have other consumers: other-container (volume my-data); reset refused before stopping the container",
  "consumers": [
    {"volumeName": "my-data", "containerName": "other-container", "containerId": "runtime-id"}
  ]
}
```

API-only consumers omit `containerId`. Runtime names may differ from API names.
Bind mounts and host directories are never reset. A bind-only target fails
without stopping. External/adopted volumes and another workload's volumes are
refused. Persistent volumes from an earlier instance require a matching workload
record and ownership token; labels alone do not authorize destructive reset.

A reused persistent container requires a matching workload record or the current
instance's verified creation ID and original UID. Arbitrary adopted containers
are not registered as owned. Without a workload record, that creation evidence
is lost at shutdown and a later instance refuses reset.

DCP holds the persistent target lease, stops and removes the target physical
container, and increments each selected ContainerVolume's requested generation.
Generation zero uses its original name; later physical names are
`<logical-name>-dcp-<generation>`. The logical name and Container specification
do not change. Ownership labels and tokens are preserved on fresh storage.
The removed persistent container's old record is discarded.

ContainerVolume `spec.generation` is nonnegative and cannot decrease.
`status.volumeName` and `status.generation` expose the selected physical mapping;
`status.message` reports selection errors. Container creation uses authoritative
Volume-controller data, not status as an input. Persistent workload records retain
the physical head so a later run cannot silently fall back to an older generation.

The Volume controller retires older owned generations without force, at selection,
subsequent reconciliation, and resource deletion. In-use generations are retained.
Ordinary adoption of labeled storage does not authorize retirement: DCP requires
current-instance creation evidence or a matching persistent workload head record.
There is no erasure deadline and reset never waits for retirement. A runtime
consumer created after preflight can retain the old data without invalidating fresh
selection. DCP does not atomically serialize arbitrary external Docker/Podman clients.
Other consumers discovered during preflight still cause refusal before target stop.

After either terminal outcome, normal Container reconciliation resumes without
changing or recreating its API resource. A running target is recreated
automatically after safe storage is available. Reset success confirms storage
reset, not application readiness; observe Container readiness separately.
Existing Start/Stop intent is honored. A Container with `spec.stop=true` stays
stopped; its ordinary start flow still requires API recreation because that
existing field is immutable.

If fresh generation creation fails after target removal, the ContainerVolume controller retries
using the preflight-verified ownership labels and token. The volume reports
`Pending` or `RuntimeUnhealthy` until verified ready, and Container startup waits
instead of letting the runtime auto-create unlabeled storage. Recovered storage
is fresh, not a rollback to the old generation. The original reset stays `Failed`.

Retry by creating a new operation bound to the current Container UID after
resolving the failure and, if needed, waiting for volume repair and normal startup.
Callers may delete terminal operation resources after observing their results. DCP
automatically deletes abandoned terminal operations one hour after their terminal
status is successfully published, not one hour after creation or runtime completion.
`finishTimestamp` records completion time and does not shorten that publication-based
retention. Pending and Running operations never expire. Expiry uses the operation UID
as a deletion precondition and preserves the same finalizer cleanup as explicit deletion.
Do not delete
ContainerVolume resources as part of reset or recovery.

Submitting the same operation name again while it exists returns `AlreadyExists`
without changing or repeating the original operation. After an ambiguous submission,
GET the original name, verify its target name and UID, and watch the existing operation
UID (also verify the original operation UID if already known). Never automatically
resubmit after a missing result: deletion or expiry makes the outcome unknown. Creating
the same name after deletion obtains a new UID and requests another storage generation.
There is no cross-instance deduplication guarantee.

Deleting an active operation requests cancellation. Its finalizer waits for
in-flight work to settle before deletion completes. Target removal cannot be rolled
back; pending generation selection remains owned by the Volume controller even
after operation deletion. HTTP cancellation alone does not cancel an accepted
operation. DCP shutdown cancels runtime work.

DCP does not support restarting controllers within an instance. On a new DCP
run, persistent volume records retain ownership tokens. Missing owned volumes
are recreated with those tokens, and Container startup verifies managed storage
before creation. Conflicting ownership fails startup. Session-only bookkeeping
does not survive shutdown.

The contract supports Docker and Podman and session/persistent lifetimes. It
requires a DCP build containing this capability and is not available in v0.26.5.
