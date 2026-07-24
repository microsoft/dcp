# V2 resource plan

This document tracks the intended direction for DCP V2 resources. The current V2 work establishes the namespace model and the first physical container/image primitives; follow-up work should continue using the design guidelines below so future resources remain consistent.

## Design guidelines

### Namespace model

- V2 resources always belong to a DCP V2 `Namespace`, except for the `Namespace` resource itself.
- V2 resources use standard `metadata.namespace` and `types.NamespacedName` for references, cache keys, reconciler state, and watches.
- V2 controllers must not perform external side effects when the namespace is missing, terminating, or not active.
- V1 resources remain cluster-scoped and can continue to run without a namespace.

### API layering

- V2 should separate physical resources from logical resources.
- Physical resources represent concrete runtime objects such as containers, images, networks, volumes, and processes.
- Logical resources represent DCP policy and user-facing behavior such as persistence, reuse, endpoint shape, and higher-level application concepts.
- Shared API fragments that are not specific to V1 or V2 should live in `pkg/commonapi`.
- Container orchestrator packages should not depend on `api/v1` or `api/v2`; API types should be converted at controller or caller boundaries.

### Controller side effects

- Reconciliation should stay fast whenever a side effect might block.
- Long-running or blocking operations should run through bounded queued work, with reconciliation recording progress and returning quickly.
- Queued action completion should enqueue a follow-up reconcile instead of directly mutating Kubernetes objects from worker code.
- Non-idempotent side effects should be guarded by lightweight in-memory data so stale or competing reconciles do not duplicate runtime work.
- DCP does not currently support controller crash recovery; in-memory progress data is acceptable because API server teardown and watcher processes clean up orphaned runtime resources after controller crashes.

### In-memory progress data

- In-memory data should be a linear progress record, not an additional state machine hidden from the resource status.
- Data records should capture only the side-effect guard/result needed for reconciliation to continue, such as a runtime ID, failure message, or current `Ready` condition reason.
- `applyTo` methods on data records should only project in-memory progress onto resource status.
- Reconciliation scheduling, state cleanup, runtime inspection, and external side effects should remain in the reconciler.
- Prefer dispatching progress handling through initializer maps keyed by condition reason when the controller has multiple progress gates.

### Status and progress reporting

- V2 resources should report coarse lifecycle with `status.phase` when a resource has a meaningful lifecycle phase.
- V2 resources should report detailed progress with standardized `Ready` conditions.
- Avoid duplicating explanatory top-level `status.message` fields when condition messages can carry the information.
- Controller status helpers should use shared target-first setters such as `setValue(&field, value)` and `setTimestamp(&field, value)`.
- Callers that need a boolean from a status helper should use `trySetX` wrappers instead of comparing `setX(...) != noChange` at call sites.

### References and watches

- V2 references should be namespace-local by default unless a cross-namespace relationship is explicitly designed.
- Controllers should watch referenced resources and enqueue dependents when updates can unblock reconciliation.
- Watches should use indexes for efficient reverse lookup, for example indexing `spec.imageRef` to find containers referencing an image.

## Current V2 foundation

- `Namespace` defines the namespace boundary for V2 resources and provides namespace-scoped cleanup.
- `PhysicalContainerImage` provides source image pull and build workflows.
- `PhysicalContainer` creates or tracks one runtime container, reports runtime status and port mappings, and references a same-namespace `PhysicalContainerImage`.
- `PhysicalContainer` and `PhysicalContainerImage` use in-memory progress data, standardized `Ready` conditions, and queued work where side effects can block.

## Follow-up roadmap

### Physical resource layer

1. Add V2 `PhysicalNetwork`.
   - Represent concrete container runtime networks.
   - Expose runtime network identity and observed network details.
   - Preserve namespace-scoped cleanup semantics.

2. Add V2 `PhysicalVolume`.
   - Represent concrete container runtime volumes.
   - Expose runtime volume identity and observed volume details.
   - Preserve namespace-scoped cleanup semantics.

3. Update `PhysicalContainer` to use physical network and volume resources.
   - Replace direct runtime network names with references to same-namespace `PhysicalNetwork` resources where appropriate.
   - Replace direct runtime volume names with references to same-namespace `PhysicalVolume` resources where appropriate.
   - Watch referenced network and volume resources so containers reconcile when dependencies become ready.

4. Decide how monitor processes should clean up physical resources after DCP crashes.
   - Define how monitor processes are configured and launched for physical resources.
   - Decide which physical resources require crash cleanup monitoring.
   - Ensure cleanup behavior works when DCP exits unexpectedly and cannot rely on controller finalizers.

5. Migrate V1 container-network tunnel proxy to V2 physical resources.
   - Keep tunnel-specific behavior in the V1 controller, including dcptun image handling, server proxy process management, TLS, tunnel gRPC calls, status, and endpoint projection.
   - Delegate common runtime container lifecycle to V2 physical resources instead of creating and managing the proxy container directly through the orchestrator.

6. Migrate V1 container resource lifecycle to V2 physical resources.
   - Keep V1-specific policy in the V1 controller, including lifecycle keys, persistent and existing container lookup, leases, compatibility status, and V1 API semantics.
   - Delegate common image/container/network/volume runtime lifecycle to V2 physical resources.
   - Avoid keeping repeated container creation, start, inspect, watch, stop, and remove logic in multiple V1 controllers.

7. Add V2 `PhysicalProcess`.
   - Launch a new process or track an existing process by PID.
   - Report observed status for the process lifetime.
   - Use the same namespace, queued action, in-memory progress, phase, and condition patterns as the other physical resources.
   - Do not assume the V1 `Executable` type will migrate to `PhysicalProcess`; IDE protocol integration may make that migration too complicated or undesirable.

### Logical resource layer

After the physical primitives are in place, add logical V2 resources that express user-facing policy on top of physical resources.

1. Add V2 logical container.
   - Provide functionality similar to V1 `Container`.
   - Own policy for persistence, reuse, and higher-level container lifecycle behavior.
   - Delegate concrete runtime work to V2 physical image, container, network, and volume resources.

2. Add V2 logical process.
   - Provide process behavior similar to V1 executable workflows, without IDE protocol functionality initially.
   - Delegate concrete process launch/tracking to `PhysicalProcess`.

3. Add V2 service.
   - Provide service behavior similar to V1 `Service`.
   - Support multiple effective addresses instead of a single value.
   - Represent cases where binding to a logical address such as localhost results in more than one concrete address, such as `127.0.0.1` and `::1`.

4. Add V2 project.
   - Represent a debuggable application.
   - Include fields for source code location.
   - Include exactly one run configuration initially: executable or container.
   - Include data required to run a debug adapter for the application.
   - Refine the design before implementation, especially debug adapter acquisition, which remains an open question.
