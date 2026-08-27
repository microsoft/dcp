# V2 resource plan

This document tracks the intended direction for DCP V2 resources. The current V2 work establishes the namespace model and the first physical container, image, and network primitives; follow-up work should continue using the design guidelines below so future resources remain consistent.

## Design guidelines

### Namespace model

- V2 resources always belong to a DCP V2 `Namespace`, except for the `Namespace` resource itself.
- V2 resources use standard `metadata.namespace`; controllers use `types.NamespacedName` for normalized references, cache keys, reconciler state, and watches.
- V2 controllers must not perform external side effects when the namespace is missing, terminating, or not active.
- V1 resources remain cluster-scoped and can continue to run without a namespace.

### API layering

- V2 separates physical runtime concerns from logical DCP policy when those concerns have distinct responsibilities or lifecycles. Not every resource requires a physical/logical counterpart.
- Physical resources represent concrete runtime objects such as containers, images, networks, volumes, and processes.
- Logical resources represent DCP policy and user-facing behavior such as persistence, reuse, endpoint shape, and higher-level application concepts.
- Shared API fragments that are not specific to V1 or V2 should live in `pkg/commonapi`.
- Container orchestrator packages should not depend on `api/v1` or `api/v2`; API types should be converted at controller or caller boundaries.

### Controller side effects

- Long-running or potentially blocking side effects must run through bounded queued work so reconciliation can record the current state and return quickly.
- Queued action completion should enqueue a follow-up reconcile instead of directly mutating Kubernetes objects from worker code.
- Results from queued runtime work must be applied to controller-owned state as deferred operations during reconciliation, ensuring each reconciliation operates on consistent state.
- Non-idempotent side effects should be guarded by lightweight in-memory data so stale or competing reconciles do not duplicate runtime work.
- DCP does not currently replay controller-owned state after controller crashes; queued side effects that create non-persistent runtime resources must stamp creator and persistence labels so startup harvesting can remove abandoned resources.

### Physical resource ownership

- A `PhysicalContainer` spec sets exactly one of `containerID`, which tracks an existing runtime container, or `container`, which contains the fields used to create one. The mutable `stop` request remains top-level.
- A `PhysicalContainerImage` spec follows the same shape: it sets exactly one of `imageID`, which tracks an existing runtime image, or `image`, which contains the source or target `image`, build, pull-policy, and retry settings.
- Both modes report the observed runtime container ID through `status.containerID`.
- A runtime container supplied through `spec.containerID` is never removed when its `PhysicalContainer` is deleted.
- A runtime container created by the resource is removed on deletion unless `retainRuntimeContainer` is true.
- Creation fails on a runtime name collision unless `replaceExisting` is true. Replacement removes the object that was resolved by name before creating and tracking the new object.
- Higher-level `session`, `persistent`, and `existing` modes remain logical policy. Logical controllers translate those modes into physical creation or reference specifications rather than copying the mode enum onto physical resources.

### Controller-owned in-memory state

- Each resource has at most one controller-owned record representing the latest known state of runtime work that has not yet been fully consumed by reconciliation.
- The record complements resource status and must not become a second, independently observable state machine.
- Controller-owned records must remain minimal: store only values needed to prevent duplicate side effects, correlate runtime events, retain queued-operation results until reconciliation consumes them, or schedule retries. Do not mirror the complete resource spec or status.
- An `applyTo` method may project controller-owned in-memory state onto the resource's status. It must not modify spec or metadata, mutate controller-owned state, schedule work, or perform external side effects.
- State transitions, work scheduling, cleanup, runtime inspection, and controller-owned state updates belong in reconciler or initializer functions.
- When controller-owned state uses a `Ready` condition reason as its current reconciliation state, dispatch reason-specific behavior through an exhaustive initializer map keyed by that reason. Unrecognized reasons must produce explicit invalid-state handling.
- Do not discard an in-memory operation result until its status projection is durable. Use the status-durable callback supplied to `SaveChanges` or `SaveChangesWithDelay` so a failed status write retains the result. If newer state can replace the record before the callback runs, acknowledge the result with an atomic conditional state-map update so a delayed acknowledgement cannot remove that newer state.

### Status and progress reporting

- V2 resources should report coarse lifecycle with `status.phase` when a resource has a meaningful lifecycle phase.
- V2 resources should report detailed progress with standardized `Ready` conditions. A condition reason identifies the specific prerequisite, operation, observation, or failure responsible for the current phase.
- Phase communicates broad lifecycle and recoverability; reason must not duplicate generic phase states such as pending or failed. The same specific reason may appear under different phases when the phase distinguishes recoverable from terminal outcomes.
- Condition messages carry instance-specific diagnostics, but consumers should not need to parse a message to determine the cause category.
- V2 status types must not define top-level `status.message` fields. Explanatory and diagnostic text belongs in the message of the condition reporting the corresponding state.
- Controller status helpers should use shared target-first setters such as `setValue(&field, value)` and `setTimestamp(&field, value)`.
- Distinguish recoverable from terminal failures. Status setters return `noChange` when a failure repeats identically, and without a watch subscription or a periodic cache resync that leaves a resource wedged with no pending reconciliation. Recoverable failures should return `additionalReconciliationNeeded` and reconcile at `LongDelay`, matching how V1 paces an unhealthy runtime; terminal failures should not requeue at all. All delays carry jitter, so retrying resources do not poll the runtime in lockstep.

### Type ownership between V1, V2, and orchestrators

- Each API version owns its own resource shapes. `api/v1` and `api/v2` deliberately declare separate copies of container fragments such as `ContainerPort`, `VolumeMount`, `ContainerBuildContext`, and `FileSystemEntry`, so V2 can evolve them without perturbing V1.
- V1 lifecycle keys are gob-encoded from V1 API types, so V1 shapes are effectively frozen. Changing a V1 type name, its exported field list, or its registration order in `initializeLifecycleHashEncoder` invalidates every existing lifecycle key and orphans running containers. `TestContainerSpecLifecycleKeyIsStable` in `api/v1/container_types_test.go` guards all three.
- `pkg/commonapi` holds only trivially simple types that are genuinely cross-cutting and are not expected to change, currently `EnvVar`, `Label`, and `PortProtocol`. V1 exposes some of these through type aliases, which preserve gob identity because an alias keeps the underlying type name and fields.
- Container orchestrators (`internal/containers`, `internal/docker`, `internal/podman`) own neutral types in `internal/containers` and must not import `api/v1` or `api/v2`. Conversion from versioned API types to orchestrator types happens at the controller boundary.

### References and watches

- V2 API reference fields use string values formatted as `<name>` or `<namespace>/<name>`.
- Every reference field's doc comment must state explicitly whether cross-namespace references are allowed.
- Controllers normalize references to `types.NamespacedName`.
- Name-only references resolve within the referring resource's namespace. Explicit namespaces must match it unless cross-namespace references are supported.
- Controllers should watch referenced resources and enqueue dependents when updates can unblock reconciliation.
- Watches should use indexes for efficient reverse lookup, for example indexing `spec.imageRef` to find containers referencing an image.

## Current V2 foundation

- `Namespace` defines the namespace boundary for V2 resources and provides namespace-scoped cleanup.
- `PhysicalContainerImage` provides source image pull and build workflows.
- `PhysicalContainer` creates or tracks one runtime container, reports runtime status and port mappings, and references a same-namespace `PhysicalContainerImage`.
- `PhysicalContainerNetwork` creates or references one runtime container network and reports its observed identity, driver, and address allocations. Its spec contains exactly one of top-level `networkID` or nested `network` creation config. Networks referenced by runtime ID are always retained. Created networks are retained when `network.retainRuntimeNetwork` is true; otherwise deletion enumerates running and stopped attachments, forcibly disconnects each container without removing it, and then removes the network. Name collisions are terminal unless `network.replaceExisting` is true, in which case the controller safely removes the specifically resolved network before creating its replacement. Runtime adapters classify their own built-in, non-removable networks, and replacement rejects them before disconnecting any attachments.
- `PhysicalContainerVolume` creates or references one runtime container volume and reports its observed name, driver, scope, mount point, and creation time. Its spec contains exactly one of top-level `volumeID` or nested `volume` creation config. Volumes referenced by runtime ID are always retained. Created volumes are retained when `volume.retainRuntimeVolume` is true; otherwise deletion removes the volume after it is no longer referenced by a container. Removal deliberately does not use force because Podman force-removes attached containers. Name collisions are terminal unless `volume.replaceExisting` is true, in which case the controller safely removes the specifically resolved volume before creating its replacement. Caller-supplied `volume.labels` pass through to created volumes, with only the internal resource UID label reserved and set by the controller.
- The physical resources use the shared `Pending`, `Ready`, `Unknown`, and `Failed` phases, specific `Ready` condition reasons, separate in-memory operation progress, and queued work where side effects can block.

## Follow-up roadmap

### Physical resource layer

1. Update `PhysicalContainer` to use physical network and volume resources.
   - Replace direct runtime network names with references to same-namespace `PhysicalContainerNetwork` resources where appropriate.
   - Replace direct runtime volume names with references to same-namespace `PhysicalContainerVolume` resources where appropriate.
   - Watch referenced network and volume resources so containers reconcile when dependencies become ready.

2. Decide how monitor processes should clean up physical resources after DCP crashes.
   - Define how monitor processes are configured and launched for physical resources.
   - Decide which physical resources require crash cleanup monitoring.
   - Ensure cleanup behavior works when DCP exits unexpectedly and cannot rely on controller finalizers.

3. Migrate V1 container-network tunnel proxy to V2 physical resources.
   - Keep tunnel-specific behavior in the V1 controller, including dcptun image handling, server proxy process management, TLS, tunnel gRPC calls, status, and endpoint projection.
   - Delegate common runtime container lifecycle to V2 physical resources instead of creating and managing the proxy container directly through the orchestrator.

4. Migrate V1 container resource lifecycle to V2 physical resources.
   - Keep V1-specific policy in the V1 controller, including lifecycle keys, persistent and existing container lookup, leases, compatibility status, and V1 API semantics.
   - Delegate common image/container/network/volume runtime lifecycle to V2 physical resources.
   - Avoid keeping repeated container creation, start, inspect, watch, stop, and remove logic in multiple V1 controllers.

5. Add V2 `PhysicalProcess`.
   - Launch a new process or track an existing process by PID.
   - Report observed status for the process lifetime.
   - Use the same namespace, queued action, in-memory progress, phase, and condition patterns as the other physical resources.
   - Do not assume the V1 `Executable` type will migrate to `PhysicalProcess`; IDE protocol integration may make that migration too complicated or undesirable.

7. Add logical resource controllers.
   - Physical controllers preserve caller-supplied runtime labels and reserve the persistence, creator-process, and internal resource UID labels they need for harvesting and uncertain-create recovery.
   - Containers and networks derive their persistence label from their physical retention field. Network harvesting intentionally ignores that label and removes orphaned networks after their creator exits so persistent networks cannot exhaust the runtime's finite default network allocations.
   - Build-created images receive persistent, creator-process, and internal UID labels through `build.labels`. Pulling resolves an expected named image and is not a runtime-object creation operation.
   - Logical controllers determine additional labels and physical retention intent when creating physical resources.

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
