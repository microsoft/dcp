# V2 resource plan

This document tracks the intended direction for DCP V2 resources. The current V2 work establishes the namespace model and the first physical container, image, and network primitives; follow-up work should continue using the design guidelines below so future resources remain consistent.

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
- DCP does not currently replay in-memory progress after controller crashes; queued side effects that create non-persistent runtime resources must stamp creator and persistence labels so startup harvesting can remove abandoned resources.

### Physical resource ownership

- Physical resources either create a runtime object or reference an existing object by runtime ID; creation fields and existing-object references are mutually exclusive.
- Runtime objects referenced by ID are never removed when the physical resource is deleted. `persistent` and `replaceExisting` apply only when creating a runtime object and are rejected for existing-object references.
- A created runtime object is removed with its physical resource unless `persistent` is true.
- Creation fails on a runtime name collision unless `replaceExisting` is true. Replacement removes the object that was resolved by name before creating and tracking the new object.
- Higher-level `session`, `persistent`, and `existing` modes remain logical policy. Logical controllers translate those modes into physical creation or reference specifications rather than copying the mode enum onto physical resources.

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
- Distinguish recoverable from terminal failures. Status setters return `noChange` when a failure repeats identically, and without a watch subscription or a periodic cache resync that leaves a resource wedged with no pending reconciliation. Recoverable failures should return `additionalReconciliationNeeded` and reconcile at `LongDelay`, matching how V1 paces an unhealthy runtime; terminal failures should not requeue at all. All delays carry jitter, so retrying resources do not poll the runtime in lockstep.

### Type ownership between V1, V2, and orchestrators

- Each API version owns its own resource shapes. `api/v1` and `api/v2` deliberately declare separate copies of container fragments such as `ContainerPort`, `VolumeMount`, `ContainerBuildContext`, and `FileSystemEntry`, so V2 can evolve them without perturbing V1.
- V1 lifecycle keys are gob-encoded from V1 API types, so V1 shapes are effectively frozen. Changing a V1 type name, its exported field list, or its registration order in `initializeLifecycleHashEncoder` invalidates every existing lifecycle key and orphans running containers. `api/v1/lifecycle_key_golden_test.go` guards all three.
- `pkg/commonapi` holds only trivially simple types that are genuinely cross-cutting and are not expected to change, currently `EnvVar`, `Label`, and `PortProtocol`. V1 exposes some of these through type aliases, which preserve gob identity because an alias keeps the underlying type name and fields.
- Container orchestrators (`internal/containers`, `internal/docker`, `internal/podman`) own neutral types in `internal/containers` and must not import `api/v1` or `api/v2`. Conversion from versioned API types to orchestrator types happens at the controller boundary.

### References and watches

- V2 references should be namespace-local by default unless a cross-namespace relationship is explicitly designed.
- Controllers should watch referenced resources and enqueue dependents when updates can unblock reconciliation.
- Watches should use indexes for efficient reverse lookup, for example indexing `spec.imageRef` to find containers referencing an image.

## Current V2 foundation

- `Namespace` defines the namespace boundary for V2 resources and provides namespace-scoped cleanup.
- `PhysicalContainerImage` provides source image pull and build workflows.
- `PhysicalContainer` creates or tracks one runtime container, reports runtime status and port mappings, and references a same-namespace `PhysicalContainerImage`.
- `PhysicalContainerNetwork` creates or tracks one runtime container network and reports its observed identity, driver, and address allocations.
- The physical resources use in-memory progress data, standardized `Ready` conditions, and queued work where side effects can block.

## Follow-up roadmap

### Physical resource layer

1. Add V2 `PhysicalContainerVolume`.
   - Represent concrete container runtime volumes.
   - Expose runtime volume identity and observed volume details.
   - Preserve namespace-scoped cleanup semantics.

2. Update `PhysicalContainer` to use physical network and volume resources.
   - Replace direct runtime network names with references to same-namespace `PhysicalContainerNetwork` resources where appropriate.
   - Replace direct runtime volume names with references to same-namespace `PhysicalContainerVolume` resources where appropriate.
   - Watch referenced network and volume resources so containers reconcile when dependencies become ready.

3. Decide how monitor processes should clean up physical resources after DCP crashes.
   - Define how monitor processes are configured and launched for physical resources.
   - Decide which physical resources require crash cleanup monitoring.
   - Ensure cleanup behavior works when DCP exits unexpectedly and cannot rely on controller finalizers.

4. Migrate V1 container-network tunnel proxy to V2 physical resources.
   - Keep tunnel-specific behavior in the V1 controller, including dcptun image handling, server proxy process management, TLS, tunnel gRPC calls, status, and endpoint projection.
   - Delegate common runtime container lifecycle to V2 physical resources instead of creating and managing the proxy container directly through the orchestrator.

5. Migrate V1 container resource lifecycle to V2 physical resources.
   - Keep V1-specific policy in the V1 controller, including lifecycle keys, persistent and existing container lookup, leases, compatibility status, and V1 API semantics.
   - Delegate common image/container/network/volume runtime lifecycle to V2 physical resources.
   - Avoid keeping repeated container creation, start, inspect, watch, stop, and remove logic in multiple V1 controllers.

6. Add V2 `PhysicalProcess`.
   - Launch a new process or track an existing process by PID.
   - Report observed status for the process lifetime.
   - Use the same namespace, queued action, in-memory progress, phase, and condition patterns as the other physical resources.
   - Do not assume the V1 `Executable` type will migrate to `PhysicalProcess`; IDE protocol integration may make that migration too complicated or undesirable.

7. Align network harvesting with `preserveOnDeletion`.
   - `harvestAbandonedNetworks` filters on `withCreator` rather than `nonPersistentWithCreator`, so it ignores `PersistentLabel` and reaps any empty DCP-created network whose creator process is gone. A `PhysicalContainerNetwork` with `preserveOnDeletion: true` is therefore still removed after a DCP crash, unlike a preserved container.
   - This asymmetry is inherited from V1. Decide whether harvesting should honor the persistent label for networks, and change V1 and V2 together if it should.

8. Retry recoverable failures in `PhysicalContainerImage`.
   - `ensurePulledImage` and `ensureBuiltImage` record an inspection failure without requesting another reconciliation, so a repeated identical failure produces no status change and leaves the image with nothing scheduled to retry it.
   - `PhysicalContainerNetwork` already follows the recoverable/terminal failure pattern described in the status guidelines. Apply the same treatment to the image controller.

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

### Log streaming

V2 has no log surface. `AdditionalTypes` is empty and no V2 resource exposes a `log` subresource, so output from a V2 runtime container cannot be read through the V2 API. This is the largest user-visible gap in the current V2 foundation.

The V1 mechanism, for reference:

- Logs are a generic `log` subresource served at `/apis/{group}/{version}/{resource}/{name}/log`, backed per resource kind by the `apiv1.ResourceLogStreamers` registry keyed by `GroupVersionResource`.
- `LogOptions` carries the query parameters. `LogStreamer` exists only because a Kubernetes storage object must be associated with a type.
- Two streamer implementations back the registry: file-based (`stdiologs`, for `Executable` and `ContainerExec`) and runtime-based (`containerlogs`, for `Container`).
- OpenAPI generation is suppressed for the subresource because the response is raw text rather than a structured object.

Whether log streams belong to physical resources, logical resources, or both is an open question. The existing source model is the strongest available evidence:

- The orchestrator layer (`internal/containers`) defines exactly two sources, `stdout` and `stderr`, because those are what a container runtime produces.
- The V1 API defines five, adding `startup_stdout`, `startup_stderr`, and `system`, which are DCP-level concepts with no runtime equivalent.
- V1 pulls and builds images inside the `Container` resource, so that output surfaces as the `startup_*` sources. In V2 that work belongs to `PhysicalContainerImage`, which can own its own stream instead.

That split suggests physical resources should expose only the streams their runtime object actually produces, while logical resources compose those streams and add DCP-level sources. Confirm the model before implementing it.

This work is cross-cutting rather than sequenced after the logical layer. The ownership decision needs enough of the logical shape to be credible, but exposing physical container and image output does not need to wait for the full logical layer to land.

1. Decide the ownership model for log streams.
   - Determine whether physical resources, logical resources, or both expose a `log` subresource.
   - Decide whether a logical resource aggregates streams from the physical resources it references, and how a caller selects between an aggregated view and a single underlying stream.
   - Decide whether V2 `startup_*` output becomes a stream on the referenced `PhysicalContainerImage` rather than a source on the container.
   - Decide whether V2 reuses the V1 source names or defines a source set per resource kind.

2. Decide where the shared log plumbing lives.
   - `ResourceLogStreamer`, `ResourceLogStreamers`, and `LogOptions` live in `api/v1` but are version-neutral in shape. Decide whether to hoist them into `pkg/commonapi`, as was done for `ResourceCreationProhibited`, or to give V2 its own copies consistent with the type ownership rules above.
   - Confirm the subresource plumbing works for namespace-scoped resources. Client-side path construction is already generic (`NamespaceIfScoped`), but every V1 resource that exposes logs today is cluster-scoped, and both `LogStreamer` and `LogOptions` return `false` from `NamespaceScoped`, so the server-side registration is unproven for namespaced kinds.

3. Add log streaming to V2 resources once the model is settled.
   - Expose runtime container output for `PhysicalContainer`.
   - Expose pull and build output for `PhysicalContainerImage`.
   - Terminate in-flight streams when the resource or its namespace is deleted, so streaming does not block namespace cleanup.
