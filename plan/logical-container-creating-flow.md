# `handleLogicalContainerPending` flow

**Status: approved for implementation. Scope is limited to the preparation flow below.**

## Responsibility and boundaries

Follow the general flow of V1 `handleNewContainer`: determine reuse versus creation, then establish the appropriate backing. The V2 difference is that the backing is a **PhysicalContainer API resource**. Runtime creation, replacement, startup, stopping, and removal belong to physical controllers.

Lifecycle determination, runtime lookup, and physical-resource declaration happen in **one Pending reconciliation**, unless an actual error or dependency requires waiting. They are not separate states or transition-only reconciliation loops.

The normal startup flow is **Pending → Creating → Running**: Pending prepares and declares the expected physical API resources; Creating observes their runtime startup until the physical container is ready. Both internal startup states currently project to the public Pending phase.

The preparation flow and its supporting discovery use owner metadata. The subsequently approved owner-discovery pattern also applies to ordinary child observation and deletion propagation; workload cleanup and physical-controller behavior are unchanged.

## Entry contracts

- Outer reconciliation has already handled logical deletion, the logical finalizer, and namespace admission. Do not repeat those checks or bypass namespace admission for a stopped resource.
- Use the logical resource snapshot supplied to this reconciliation. Stop or deletion arriving during a lookup is handled on a subsequent reconciliation; do not refresh the parent to chase it.
- Rely on validated mode/configuration rules: session and persistent modes have a container template; existing mode does not.
- Discover children by the logical resource's controller-owner UID, within its namespace, using a cached owner index. Recorded child UIDs check identity after discovery; they are not discovery keys.
- Use in-memory records of actual work to avoid repeating an already-established child creation. Do not add a preparation checkpoint, creation-uncertainty flags, or retain per-pass calculations.

## Flow

### 1. Discover the owned physical container

- Zero owned children and no recorded UID permits creation.
- Zero owned children with a recorded UID means wait and requeue; do not create a replacement because the cache has not returned the expected child.
- One owned child records its UID on first observation, or must match the already-recorded UID.
- More than one owned physical container is a terminal error during normal reconciliation. Do not choose an arbitrary child.

An existing container skips runtime selection and container creation. If its creation template uses the logical resource's inline image, Pending must also acquire the single owned PhysicalContainerImage before handing off to Creating. Cached image absence waits; multiple owned images are terminal, just like multiple owned containers.

### 2. Establish creation policy

Obtain the effective mode once and use its behavioral methods, following V1:

- `ShouldReuseExisting()` determines whether to inspect the named runtime container.
- `ShouldCreateIfMissing()` determines whether a new runtime container may be requested.
- Persistent mode acquires the shared creation lease before making creation/replacement decisions. Lease contention or an acquisition error returns through the existing retry/error path.

Lease acquisition is synchronous. Retain the acquired lease because it represents actual work that spans reconciliations, not because preparation is an asynchronous operation.

### 3. Inspect an existing runtime when reuse is permitted

Look up the requested runtime name using the established inspection helper. Keep the inspected result local.

- A lookup error is logged and requeued.
- A missing runtime proceeds to the mode decision below.
- Do not add a separate logical runtime-health probe or a logical work queue.

### 4. Decide the backing locally

| Mode / observation | Decision |
|---|---|
| Session, stop requested | No runtime creation. Finish with the stopped outcome. |
| Session, otherwise | Request a new runtime through a physical creation template. |
| Persistent or existing, runtime found and stop requested | Declare a physical ID reference with `Stop = true`. Do not replace the runtime or acquire an image for a new container. |
| Existing, runtime found | Declare a physical ID reference. |
| Existing, runtime missing | Remain Pending and requeue; if stop was requested, finish stopped without creating a backing. |
| Persistent, unmanaged runtime found | Reuse it through a physical ID reference. Do not claim workload cleanup ownership. |
| Persistent, DCP-managed runtime found, matching lifecycle key, running | Reuse it through a physical ID reference. Preserve existing workload ownership. |
| Persistent, DCP-managed runtime found but key changed or runtime is not running | Request a physical creation template with replacement enabled. The physical controller performs the replacement. |
| Persistent, runtime missing | Request a new physical creation template. |

Calculate the desired lifecycle key when needed to compare a managed candidate or create a new container. Reuse an adopted container's observed key where appropriate. Generate a session runtime name only when a new unnamed container is actually needed.

The candidate ID, inspected labels, key comparison, replacement decision, generated name, and desired physical spec are **local values**, not updates to retained controller data.

### 5. Declare the required physical resources

For an ID reference:

- Set `PhysicalContainerSpec.ContainerID` and propagate the observed logical stop intent.
- Do not create an image dependency or apply creation-only monitor/configuration settings.

For a creation template:

- Use the declared external image reference, or declare the owned inline PhysicalContainerImage.
- Acquire the owned inline image and record its UID before handing off, including when the physical container already exists. This requires the image API resource, not completion of its pull or build.
- Project startup logs from that image during Pending, including while physical container declaration is retrying.
- Populate the physical template from the validated logical configuration and the local creation decision.
- Set runtime retention according to session versus persistent policy.
- Preserve the required configuration and labels. Do not silently drop unsupported/unresolved logical-network attachments; that is a real dependency wait.

Use the established deterministic API names for new child declarations, but do not infer an existing child's name from those conventions. Owner metadata determines discovery. Set owner references and both required finalizers atomically when creating owned children.

API construction/create helpers may return resources and errors. They must not communicate preparation results by mutating logical state and requiring the caller to inspect those mutations. Logical controllers have no asynchronous-work progress field. Retryable errors retain the current state, error diagnostics, and retry deadline; they are not separate states.

### 6. Handle the API write outcome

- Successful Create supplies the child UID to retain.
- After an ambiguous Create failure or `AlreadyExists`, perform an immediate uncached owner lookup. A single confirmed child supplies the UID and completes the same creation step; multiple owned children are terminal.
- If the write outcome cannot yet be established, report the error and requeue. The next owner query discovers the child independently of whether a Create response supplied its UID. Do not introduce preparation snapshots, uncertainty flags, or a replay engine.
- Ordinary cached absence is not terminal disappearance. Wait and requeue rather than forcing fresh reads or repeating confirmed non-idempotent work.
- Once a UID is known, use it for identity checks; do not repeatedly police controller ownership.

### 7. Hand off to observation

Record the accepted child identities and the lifecycle key associated with that backing. Transition to Creating for runtime observation, or Stopping when the supplied logical snapshot requests stop, only after acquiring both expected children.

This does **not** mean the public phase becomes Running merely because the API child exists. Subsequent child updates determine runtime readiness and completion. Do not immediately fetch a newer physical status or wait for a particular generation in this method.

For adoption or a stopped outcome with no creation work, release any creation lease without registering workload ownership. For requested runtime creation/replacement, retain the lease through the existing creation-completion and ownership-settlement flow.

Workload cleanup registration is not part of creation preparation. Only confirmed runtime creation/replacement may establish new cleanup ownership; adopting a runtime reference must not create or overwrite that ownership.

## Owner-based deletion propagation

Deletion enumerates and requests deletion of **every** owned physical container, even if normal reconciliation failed because there were duplicates or no child UID was ever recorded. A recorded UID must not hide another owned child or prevent its cleanup.

Observe physical cleanup through the existing finalizer protocol, settle any tracked creation ownership/lease work, and release logical holds. Apply the same owner-based enumeration to owned inline images after container cleanup. Finalize the logical resource once its owned backings and remaining housekeeping are gone.

Deletion does not select a single child or apply the normal single-child cardinality restriction.

## Keep out of this method

- Runtime log observation and physical container runtime-status projection. Pending projects startup logs from the owned image it acquires; shared management performs no state-specific child lookup or transition.
- Direct runtime create, remove, start, or stop commands.
- Parent freshness rereads and mid-reconciliation interruption machinery.
- Lifecycle/lookup substates, `creationPrepared` flags, progress-as-return-value helpers, or cached preparation scratchpads.
- A new generic transaction/recovery framework or unrelated controller cleanup.

The handler owns its state-machine outcomes: continue, wait/requeue, fail, or hand off. Shared management persists those outcomes and applies the common state/status projection; child observation and startup-log projection remain in the relevant state handler.

## Verification after plan approval

Cover session creation, persistent creation and replacement, managed/unmanaged adoption, existing-mode absence, initial stop, ambiguous and `AlreadyExists` API creation, and stop/deletion arriving during lookup. Verify discovery of differently named owned children, terminal duplicate detection with or without an existing physical container, and deletion of all owned children without a previously recorded UID. Verify that cached image absence keeps the logical container in Pending even when its physical container exists, and startup logs remain available during container declaration retries. Successful preparation and backing declaration must not require artificial extra reconciliation passes, and adoption must never grant workload cleanup ownership.

**Approval is limited to this preparation flow and its directly required supporting changes.**
