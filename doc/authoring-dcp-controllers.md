# Authoring DCP controllers

Developer Control Plane controllers are, for all practical purposes, Kubernetes controllers. There is a lot of documentation and guidance available on the topic of authoring Kubernetes controllers, but most of it does not go beyond conceptual and introductory level, and can be somewhat outdated. This document is intended to address this gap. It provides enough context and details to get someone just starting the work in the DCP codebase up to speed quickly.

# Kubernetes resources

At the highest level of abstraction Kubernetes can be thought as a document database. Various kinds of documents, or **resource kinds** in Kubernetes lingo, can be stored in the database and accessed via a RESTful API. Kubernetes ships with a bunch of built-in, [standard resource types (kinds)](https://kubernetes.io/docs/reference/kubernetes-api/) such as Pod, ReplicaSet, Service, and others, but it is an extensible system, and users can define their own types of resources via Custom Resource Definitions, or CRDs. These CRDs make Kubernetes "understand" new types of resources, and expose them for querying and editing via the API endpoint exactly like standard resources.

Instances of resources are created simply "resources", or "objects". The latter term is especially popular in the context of accessing Kubernetes via API.

> Kubernetes convention is to start the names of resource kinds with uppercase letter (a Pod, Service, Executable etc). This makes it clear whether we are talking about a resource kind (e.g. Executable) vs an instance of a kind (executable = "some specific executable").

## Resource grouping and versioning

Kubernetes resource kinds are organized into named groups, the idea being that related resources will belong to the same group. The resource group concept is similar to a namespace concept in programming languages; it helps avoid naming conflicts for different resource kinds, but otherwise does not place any restrictions on resources. For example, resources from different groups can freely refer to one another.

> In DCP all resource kinds belong to `usvc-dev.developer.microsoft.com` group.

> API groups should not be confused with [Kubernetes namespaces](https://kubernetes.io/docs/concepts/overview/working-with-objects/namespaces/). Namespaces are a means for grouping Kubernetes objects (not kinds). They often serve as a security boundary, isolating resources that belong to different applications.

Resource kinds can have multiple versions (also called **API versions**). Every object in the system has its version information stored with it; this version is called "storage version" for this object. Kubernetes design guidelines emphasize that clients should be able, to the largest extent possible, use their preferred API version when working with an object, even if the storage version is different than the client API version. Kubernetes has facilities to support this scenario, but it requires additional effort by the programmer to translate versions on the fly as necessary.

> In DCP all resources have a single version, `v1`.

---

# API server and data store
The part of Kubernetes that orchestrates all the work performed by a Kubernetes cluster is called control plane. You can read [Kubernetes control plane conceptual overview](https://kubernetes.io/docs/concepts/overview/components/#control-plane-components) to learn more about Kubernetes control plane and all its components. The most important control plane components are the API server and the data store (also called backing store).

## API server
The **API server** is the control plane component that exposes the Kubernetes API and handles requests that query and modify resources.

> In DCP we use [an API server library from the Tilt project](https://github.com/tilt-dev/tilt-apiserver) to implement our API server. The API server exposes a Kubernetes-compatible interface, but does not "know" about any standard Kubernetes resource types. Instead, it is configured to recognize DCP-specific resource types for describing the developer workload, such as Executable, Container etc.

## Data store
The **data store** manages resource data. Kubernetes uses [etcd](https://etcd.io/docs/) as its default data store, a consistent and highly available key-value database.

> DCP does not need the high availability of etcd, and does not need to persist data beyond the lifetime of the dcp daemon, so it uses an in-memory data store instead. In future, if workload data persistence is required, DCP could switch to a single-node etcd instance for storing workload data on disk.

---

# Controllers and reconciliation

The objects stored in Kubernetes data store serve as **a model** for the real world. That is, the objects describe how the world should be, and store data about their corresponding real world entities. In DCP the "real world" is developer workload (application) that DCP is supposed to run and monitor. For example, when an Executable object is created, its data must include the path to the program that DCP should run. When DCP starts the program, it will add information about the running program to the Executable object (e.g. its process ID when the program is running, and exit code when the program exits).

The part of DCP that is responsible for making the developer workload correspond to the model is called **a controller**. Typically, every kind of resource has its own controller, all running independently and in parallel. Each controller registers with the API server and gets notified about any changes to object of a kind that the controller handles. The controller compares the model with the workload on the machine and performs actions that make the workload correspond to the model. It is also watching the workload for state changes, and updates the model objects so that they reflect the state of the workload.

> When talking about nonstandard Kubernetes resource kinds, a combination of custom resource definition (CRD) and its associated controller is often called **an operator**.

This two-way synchronization between the Kubernetes object model and "the real world" (the running workload in DCP case) is called **reconciliation**. The main part of every controller code is a "reconciliation function" that is called whenever a change is detected, either to the model, or the real world.

> Although the reconciliation function is typically triggered by a change, there is no guarantee that the function will be called within any specific amount of time. Consequently, the controller should not assume that the change that triggered reconciliation accurately represents the latest state of the model, or the real world. In other words, it is *not true* that Kubernetes will present the controller with a complete history of changes for every object the controller is interested in. In Kubernetes documentation this assumption is called **edge-based behavior** and is listed specifically listed as a mistake.
>
> Instead, the controller should ensure that it has the most recent information about both the model and the world, and only then decide what the next action (if any) should be. In Kubernetes documentation this is referred to as **level-based** behavior.

---

# Resource spec and status

Every resource kind in Kubernetes consists of **spec** part and **status** part. The spec has also two parts: the part common to all object called **object metadata** and the part that is specific to the resource kind.

## Object metadata

Most important properties that are part of object metadata are:

| Property | Description |
| --- | --------- |
| `name` | The name of an object. Must be unique within namespace, or within the whole cluster (if the object kind is not namespaced). <br/><br/> Most DCP kinds are not namespaced. |
| `namespace` | The namespace to which the object belongs (or empty, if the kind is not namespaced). |
| `labels` | A set of key-value pairs that are used by system operators to categorize objects. This is used to perform management operation on multiple objects (e.g. "set the property `X:=123` on objects with label `layer==frontend`), and to establish a loose relationship between objects (e.g. use Pods with label `purpose==worker` to process requests for Service `worker-svc`). |
| `annotations` | Annotations is a set of key-value pairs that are set by tools to store and retrieve arbitrary metadata associated with an object. Unlike labels, they are not meant for human use. |
| `finalizers` | Contains a list of identifiers for components that must "process" an object before it is deleted from the data store. |
| `ownerReferences` | List of references to objects that "own" this object. When all owners are deleted, this object will be garbage-collected by the system automatically. |
| `creationTimestamp` | A timestamp representing the server time when the object was created. Set by the system. Read-only. |
| `deletionTimestamp` | A timestamp representing the server time when the object will be deleted. Used when "graceful deletion" is requested by the client; it allows real-world resources and finalizers some time for cleanup work. Set by the system. Read-only. |
| `generation` | Generation is a monotonically increasing integer representing the version of the *spec* part of the object data. Whenever spec changes, the generation number is increased by the system. Read-only. |
| `resourceVersion` | Resource version is an opaque string representing the digest (hash) of all object data. Set by the system and intended to use for optimistic concurrency checks (like HTTP ETags). Read-only. |

A full list of object metadata properties can be found [in Kubernetes documentation](https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/object-meta/).

## The spec

The spec part of object data represents **the desired state of the world**. This data is specific to the object kind and is meant to be set by clients. Controllers usually do not modify any spec data; they just read it and react to changes.

## The status

The status represents **how the world actually is**. More precisely, status is the state of the world as witnessed by the controller during last execution of reconciliation function.

Status data should be created and modified by controllers. Clients read the status to get information about the world, but do not modify status data.

Kubernetes does not prescribe the format for spec and status, but there are some conventions that Kubernetes tools rely on to make the status information more user-friendly. For more information about these conventions refer to [relevant chapter in the Kubernetes API conventions document](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#spec-and-status).

---

# Libraries for controller authoring

DCP API server and its core controllers are implemented in Go. Users of Go have many choices for creating a controller/operator. The most popular choices are:

| Library/tool | Description |
| --- | --------- |
| [client-go](https://github.com/kubernetes/client-go/) | `client-go` is the lowest-level library for writing controllers that is in widespread use. It has support for both strongly-typed and unstructured ("dynamic") clients and has a sophisticated mechanism for caching results of queries and tracking object changes ("informers"). <br/> <br/>For more information see below, and visit [sample controller repository](https://github.com/kubernetes/sample-controller/blob/master/docs/controller-client-go.md) or [Kluster example](https://github.com/viveksinghggits/kluster). |
| [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) | `controller-runtime` adds a layer of abstraction on top of `client-go` that encapsulates routine operations for the controller and allows the controller author to focus on logic specific to the kind the controller manages. It also helps with authoring admission and conversion webhooks that are used for implementing advanced validation, authorization, and transformation logic for your resource kinds.<br/> <br/> A good example of controller leveraging `controller-runtime` is [Scott Rigby's Proposal sample](https://github.com/scottrigby/how-to-write-a-reconciler-using-k8s-controller-runtime/tree/main/cfp). |
| [kubebuilder](https://book.kubebuilder.io/introduction.html) | `kubebuilder` is a code generation scaffolding tool that leverages `controller-runtime`. It is meant to get you quickly started with your controller implementation and allow rapid iteration when your spec- and/or status data change. |
| [Operator Framework](https://operatorframework.io) | Operator Framework is a set of developer tools and Kubernetes components that aid in operator development, handling such tasks as operator installation, update and management. It leverages `kubebuilder` and other tools. |

In DCP we rely mostly on `container-runtime` and associated code generation tools. We also use types and functions form `client-go` library.

## Most important library primitives

In this paragraph we briefly describe the most important types in the Kubernetes client libraries. The information here is intended to help you understand what the intention of the particular DCP code snippet is, based on what library types it may use.

### ClientSet
At a most basic level, the `client-go` library has the facility to generate a strongly-typed client for a given resource kind (in the form of a Go structure). This type of basic client is called a `ClientSet`, and it enables CRUD operation on a single kind, by talking to Kubernetes API server directly.

The "set" part comes into play when you need to work with more than one kind of resource. `ClientSet` instances can be merged to form an aggregate `ClientSet`, which "knows about" multiple resource kinds, including all standard Kubernetes kinds.

To use a `ClientSet`, one typically needs to use the `code-generator` tool (see below) to generate boilerplate code that the `client-go` library needs.

### Listers and Watchers
`Lister` and `Watcher` are low-level interfaces for, respectively, listing objects that meet given criteria, and for receiving change notifications about objects. They appear often in the context of other library interfaces, and are useful, but we tend not to use them directly in DCP code. Instead, we rely on the more performant `Informer` type, see below.

### Client (controller-runtime)
The `controller-runtime` library introduces a `Client` interface that exposes complete functionality for querying and updating objects, subsuming several types from the `client-go` library. Here is the abbreviated definition of the `Client` interface:

```go
type Client interface {
  Reader
  Writer
  StatusClient
  Scheme() *runtime.Scheme
  RESTMapper() meta.RESTMapper
}
type Reader interface {
  Get(ctx context.Context, key ObjectKey, obj Object) error
  List(ctx context.Context, list ObjectList, opts ...ListOption) error
}
type Writer interface {
  Create(ctx context.Context, obj Object, opts ...CreateOption) error
  Delete(ctx context.Context, obj Object, opts ...DeleteOption) error
  Update(ctx context.Context, obj Object, opts ...UpdateOption) error
  Patch(ctx context.Context, obj Object, patch Patch, opts ...PatchOption) error
  DeleteAllOf(ctx context.Context, obj Object, opts ...DeleteAllOfOption) error
}
type StatusClient interface {
  Status() StatusWriter // For updating status sub-resource
}
```

`controller-runtime` clients require much less boilerplate code  than `client-go` clients do, but there is still some (primarily for deep-copying of objects). The library has a dedicated code generator called `controller-gen`, see below.


### Informers
An `Informer` combines the ability to receive and handle notifications about changes to objects of a particular kind with an in-memory cache with indexed lookup. This places minimal load on the API server and increases performance. Under the covers the Informer fills its object cache upon startup and then signs up for change notifications for the desired object kinds. It will also handle connection failures and various kinds of common API server errors.

> Caveat: unlike `controller-runtime` `Client` objects, objects returned by `Informers`, `Listers` and `Watchers` are direct pointers to client cache content. The controller cone must make a copy before modifying them.

### Scheme and RESTMapper
These two types play a role of configuration/helper data that enable calling Kubernetes API server REST endpoint.

`Scheme` contains the mapping between resource group, version, and kind (`GroupVersionKind` struct, or GVK) and the Go struct that holds the data for the objects of that kinds. It is used to serialize and deserialize data exchanged with the API server.

`RESTMapper` maps the group-version-kind information to URL paths used by the API server. In other words, it tells the client library what URL (path) to use when the controller code requests an operation on an object of particular kind. It also tells the client whether a particular GVK is namespaced or cluster-scoped.

> Kubernetes has an established convention for "primary" resource paths (by primary we mean a canonical path to access a single object of a given kind). The convention is:
> ```
> /apis/{group}/{version}/namespaces/{namespace}/{kind}/{object ID}
> ```
> or, for non-namespaced resources
> ```
> /apis/{group}/{version}/{kind}/{object ID}
> ```
> That said, the same resource kind can be associated with different paths (e.g. when dealing with sub-resources). That is why extra information about GVK-to-path mapping is necessary.

The most commonly used RESTMapper is `DynamicRESTMapper`, which discovers resource kinds via API server discovery endpoint. This works for standard Kubernetes types and Kubernetes `CustomResourceDefinitions` (CRDs), but the DCP API server does not "know of" any standard Kubernetes types, including `CustomResourceDefinitions`. That is why DCP resource definitions are installed programmatically into the default `SchemeBuilder` as part of API server startup.

### Controller Manager
A controller manager owns an cache and a set of Informers. A common pattern with Kubernetes controllers is to run multiple controllers in a single process, with a single controller manager. During startup each controller will register with manager and specify what objects to watch, and what properties of these objects are of particular interest and need to be indexed. The controller manager will in turn set up a shared object cache and a shared set of Informers for all controllers. This minimizes the number of interactions with Kubernetes API server.

### Work queue
`controller-runtime` library includes an implementation of a rate-limiting work queue. When a change notification comes in, the controller reconciliation function is not invoked immediately. Instead, the ID of the object that has changed is put into the work queue, which is then examined by the controller on a scheduled basis. This allows the controller to process a set of quickly-occurring changes to an object just once. The work queue can be used to manage the real world changes as well, simplifying controller code.

## Code generation (`controller-gen` and `code-generator`)

Kubernetes custom resources and controllers typically use generated code for many boilerplate parts. Most commonly used generators include:

- CRD generator (for generating CRD manifests),
- object/deep copy generator (for generating deep copy code used by `client-go` runtime),
- OpenAPI schema generator
- webhook generator (for authoring validating and mutating webhooks)
- RBAC generator (for generating roles and setting up secure object access for generators).

The result is either Go source files, or YAML-format manifests (with Kustomize overrides) that can be applied directly to a Kubernetes templates.

In DCP we use a code generation tool associated with `controller-runtime` library, called  [controller-gen](https://github.com/kubernetes-sigs/controller-tools/blob/master/cmd/controller-gen/main.go#L130). It is a set of code generators that satisfy basic needs of controller authors using `controller-runtime` library. We use this generator to generate "deep copy" methods required for all Kubernetes objects.

Another popular code generation tool for Kubernetes is called `code-generator`; it is also a set of code generators, targeting the lower-level `client-go` library. We use the [`openapi-gen` generator](https://github.com/kubernetes/kube-openapi) from that set to generate OpenAPI schemas for our types.

Both `controller-gen` generators and `code-generators` are driven by specially-formatted comments in the source code. The comments start with plus (+) sign, followed by the tag that the generator recognizes, for example
```go
// +k8s:openapi-gen=true
```
instructs the `openapi-gen` generator to generate OpenAPI schema for the following `struct`. The documentation for these tags is very scarce and one often has to refer to the generator source code to find out what is available--sorry!

# Controller implementation

## General rules

### Objects are always slightly stale
All standard Kubernetes client libraries use a cache, so objects that the client sees can always be somewhat out of date. Moreover, Kubernetes does not make any guarantees in terms of how soon controllers will be notified about a change to objects they are watching, so even if the cache is bypassed, there is still no such thing as "object data that is guaranteed fresh" in Kubernetes. The best approach is to keep a mindset that all data "seen" by controller code is <u>a snapshot</u> from (hopefully near) past.

## Working with the object data cache

### The startup cache sync
Objects retrieved via `controller-runtime` `Client` APIs are fetched through the [object cache](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/cache). Specifically, the default client is a `DelegatingClient` that delegates `Get` and `List` calls to the cache. This means the read operations are usually very fast <u>except during startup</u>, when the first action of the runtime is to fill the cache and get the `Watches` going. Also, by default, the `Watches` created by `controller-runtime` are cluster-scoped, meaning they watch objects across all namespaces. As a result, if one is not careful, the initial cache sync may take several seconds with a real Kubernetes cluster. This is not a concern with DCP, but still something to keep in mind.

### Adjusting the default caching behavior
There are a few mechanisms in `controller-runtime` that allow changing the default caching behavior:

- When creating a `DelegatingClient`, certain object kinds can be configured to always be read directly from the API server.
- `Watches` can be restricted to a give set of namespaces by using `cache.MultiNamesapceCacheBuilder`, or setting `cache.Options.Namespace`.
- Watches can be filtered (e.g. by label) per object kind by using `cache.Options.SelectorsByObject`.
- `APIReader` can be used for ad-hoc, cache-bypassing queries (although the need for that should be rare). Also keep in mind that the `APIReader` call will result in a quorum-read from the underlying etcd storage, which might be costly with a heavily-used cluster, and especially if a lot of data is returned, so always use namespace and label filters if possible.

### Object updates and the cache
The cache is not used for data-modification functions (`Create`, `Update`, `Patch`, and `Delete`). This means you should usually do the update towards the end of the reconciliation function, and then return. Do not use `Client` APIs to read the object again, because most likely the cache will not be updated yet and you will read stale data. If you need the updated object data (e.g. generation number), the data-modification functions will save these (new) data into the object passed to them.

Another point to remember is that even if a particular update initiate by a controller is the last update that has been applied to an object, the object data may not be *exactly the same* as what the controller sent, due to the presence of mutating webhooks. We do not use mutating webhooks in DCP, but this is true for regular Kubernetes clusters.

An advanced technique employed by some controllers is to anticipate `Watcher` events for newly created or updated objects by storing expected event data about them (context) in memory and processing them in a simplified manner. For example, a controller might create a bunch of child objects of a kind that it also watches; it can expect receiving a reconciliation function call for each created child object. This controller also "knows" that a bunch of children have been just created, so it can take this information into account when deciding whether to create more children, even if the cache has not been updated yet with the new child data.

### `SharedInformerFactory` does not produce globally shared `Informers`
Although `controller-runtime` uses `client-go` primitives under the covers, it does not use `client-go` default object cache and pool of `Informers`. In particular, `Informers` created by `SharedInformerFactory` that is part of `client-go` will not be using the `controller-runtime` object cache, and are completely separate from `Informers` used by `controller-runtime`.

A rule of thumb to avoid such issues is to consistently use for data retrieval either `client-go`, or `controller-runtime`, but not both.


## Metadata-only queries

`controller-runtime` clients can be used to easily retrieve metadata-only objects and lists. This is useful for efficiently checking if at least one object of a given kind exists, or retrieving metadata of an object if one is not interested in the rest (e.g., spec/status). This saves network traffic and cpu/memory load on the API server and client side. If the client fully lists all objects of a given kind including their spec/status, the resulting list can be quite large and consume a lot of memory. That's why it's important to carefully check, if a full list is actually needed or if metadata-only list can be used instead. For example:

```go
var (
  ctx     context.Context
  c       client.Client                         // "sigs.k8s.io/controller-runtime/pkg/client"
  podList = &metav1.PartialObjectMetadataList{} // "k8s.io/apimachinery/pkg/apis/meta/v1"
)
podList.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PodList")) // "k8s.io/api/core/v1"

if err := c.List(ctx, podList, client.InNamespace("my-namespace"), client.Limit(1)); err != nil {
  return err
}

if len(podList.Items) > 0 {
  // namespace contains at least one pod
} else {
  // namespace doesn't contain any pods
}
```


## Updating Kubernetes objects

### Modifying objects returned by `Client` API is safe
The `obj Object` parameter used by `controller-runtime` client APIs is actually a pointer to an object structure. It means the client will modify the object as necessary and no copy will be made. It also means the object is owned by the code that called the `Client` API and can be modified freely, and the modifications will not affect any data cached by the `controller-runtime` library.

This pattern of passing objects "by reference" is commonly used in controller code, not just when calling client APIs, but also for passing objects between functions belonging to controller code. It is efficient and results in straightforward code, but is not goroutine-safe. If multiple goroutines need to work on the same object, one must use synchronization techniques to ensure proper happens-before relationships, or make a deep copy of the object.

### Handling conflicts

Kubernetes controllers should always assume that some other client might have changed the object between the time it was retrieved by the controller and the time an update was attempted. Most requests should include `resourceVersion` metadata property in the update/patch requests to have the update fail if it was made based on stale data.

> The `resourceVersion` property can vary depending on what data store the Kubernetes cluster is using. Do not assume a particular format of this property, or try to parse it to determine which data is older/newer.

**The best way to handle conflicts is usually not to have a special handling for them at all.** Instead, the controller should just return the error from the reconciliation function, and let the default controller manager logic take care of refreshing the object cache and calling the reconciliation function again with new data.

If the update is urgent, one can test if the error was a conflict (HTTP status code 409) via [`errors.IsConflict`](https://pkg.go.dev/k8s.io/apimachinery/pkg/api/errors#IsConflict) and retry. There is also a [`RetryOnConflict`](https://pkg.go.dev/k8s.io/client-go/util/retry#RetryOnConflict) utility function that can wrap the (get data--compute changes--update) sequence, retrying it with exponential back-off, but given "objects are always slightly stale" general rule, retry-on-conflict should be reserved for special, well-defined cases.

### Objects owned by a controller
A special situation arises if a controller creates objects that are not meant to be updated by anything else. These objects are "owned" by the controller. A common example is a need to create "children" objects for the root object the controller manages (for example, the standard Kubernetes Deployment controller creates ReplicaSet objects for deployments).

In case of objects fully owned by a controller, it can be beneficial to update them by patching the spec without optimistic concurrency control (not including `resourceVersion` metadata). Since there is only one writer, this is safe, and avoids unnecessary retries. A separate controller can still update the status. As long as there is clear division of responsibility, the data will always remain consistent.

Another way of dealing with optimistic concurrency is to use <u>immutable child objects</u> with deterministic names. This involves a naming convention, where children are named according to formula `(parent name)-(hash of parent data)-(sequence number)`, where `(hash of parent data)` is the hash of parent data that is relevant to the child. These children never change once created, and it is enough to compare the hash of parent data with the child name to know, which ones are current, and which one are stale. Again, this assumes that a controller fully owns the child objects and will be the only entity that modifies their spec.


### `controller-runtime` object update APIs
The following snippet demonstrates different ways an object update can be done with `controller-runtime`:

```go
var (
  ctx   context.Context
  c     client.Client
  obj   *apiv1.SomeObject
)

// update
obj.Spec.SomeProperty = "alpha"
// more changes to obj...
err := c.Update(ctx, obj.DeepCopy())

// merge patch
patch := client.MergeFrom(obj.DeepCopy()) // Establish baseline
obj.Spec.SomeProperty = "alpha"  // Change that needs to be included in the patch
// more changes to obj...
err = c.Patch(ctx, obj.DeepCopy(), patch)

// server-side apply patch
obj.Spec.SomeProperty = "alpha"  // Change that needs to be included in the patch
// more changes to obj...
err = c.Patch(ctx, obj.DeepCopy(), client.Apply, client.ForceOwnership, client.FieldOwner("controller-name"))
```

Note that:
- Patch request only contains changes made to the object between the call to `*MergeFrom()` and the call to `Patch()`. This is also why the code should pass a deep copy of the object to `*MergeFrom()`--otherwise there will be no "object delta" at the time when `Patch()` is called.
- Merge patch requests conform to RFC 7396 JSON Merge Patch spec (see [JSON Patch and JSON Merge Patch overview](https://erosb.github.io/post/json-patch-vs-merge-patch/) for introductory information). The most important caveat about JSON Merge Patch is that if a list is included in the patch, entire list will be replaced; there is no way to insert/update/delete individual elements.
- `Update()` requests involve the whole object, including metadata, so they participate in optimistic concurrency by definition. By default, `Patch()` requests do not include `resourceVersion` and do not participate in optimistic concurrency, but it can be explicitly enabled for merge patch:
    ```go
    // json merge patch + optimistic locking
    patch := client.MergeFromWithOptions(obj.DeepCopy(), client.MergeFromWithOptimisticLock{})
    // ...
    ```
- Update methods (both `Update()` and `Patch()`) take a copy of the current object data (`DeepCopy()` is used to create the copy). Why this is helpful will be explained in the next paragraph, about updating sub-resources.

### Updating sub-resources
For those scenarios when the update <u>applies only to a sub-resource</u>, `controller-runtime` offers a specialized `SubResourceClient`. The most common example of a sub-resource is the object status. Indeed, updating the status is perhaps the most common object update scenario for controllers. This is why the `controller-runtime` has a `Status()` helper method for accessing the status sub-resource client right off of the root object client. Here is how a controller can use it:

```go
var (
  ctx   context.Context
  c     client.Client
  obj   *apiv1.SomeObject
)

// update
obj.Status.SomeProperty = "bravo"
// more changes to obj.Status (only) ...
err := c.Status().Update(ctx, &obj)

// merge patch
patch := client.MergeFrom(obj.DeepCopy()) // Establish baseline
obj.Status.SomeProperty = "bravo"  // Change that needs to be included in the patch
// more changes to obj.Status (only) ...
err = c.Status().Patch(ctx, obj.DeepCopy(), patch)
```

The `SubResourceClient` exposes `Create()`, `Update()`, and `Patch()` methods, just like the "root" client does. `Delete()` is absent, because sub-resources have no independent life.

> Caution: you must use the methods of `SubResourceClient` to update status data. Updates to status will not be included when methods of the "root" client is invoked!

If changes to an object involve both object data and object status, care needs to be taken that data is not inadvertently lost when making the two update calls:

```go
var (
  ctx   context.Context
  c     client.Client
  obj   *apiv1.SomeObject
)

// This uses merge patch only; full update would use the same sequence.
patch := client.MergeFrom(obj.DeepCopy()) // Establish baseline
obj.SomeProperty = 42 // Change that needs to be included in the patch
// more changes to object data ...

obj.Status.SomeProperty = "bravo"  // Status change that needs to be included in the patch
// more changes to obj.Status ...

// Use DeepCopy() to ensure that status changes are preserved inside obj...
update := obj.DeepCopy()
err = c.Patch(ctx, update, patch) // Saves object changes
if err != nil {
  // Handle error...
}

// "update" now reflects the state of the object as seen by the API server after updating object data.
// It does not contain status changes!
update = obj.DeepCopy()
err = c.Status().Patch(ctx, update, patch) // Now save status changes
if err != nil {
  // Handle error
}

// obj still contains "desired state" of the object
// "update" now contains the (fully) saved object data.
```
If necessary, the copy of the

### Server-side apply patch
The [server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/) type of patch is a newer option that leverages the concept of field ownership for concurrency control. Essentially, each actor that modifies the object can mark the fields it "owns" via `managedFields` metadata property. When another actor tries to update an owned field to a different value, an update conflict will occur. The actor can then choose to either:
1. Force the update and grab the ownership of the field, or
2. Remove the field from the patch content and retry, or
3. Keep the field in the patch content, but set it to the latest value reported by the API server and assume "shared ownership" of the field, then retry.

Controllers typically use option (1); options (2) and (3) are reserved for human administrators using tools such as `kubectl`.

The limitation of server-side apply patch is that there is no way to delete any field that is not already owned by the controller supplying the patch. The workaround is for the controller to use an update operation or the "JSON merge" patch type.

The advantages of a server-side apply patch method are:
- Because concurrency checks are made per-field, multiple actors can update different fields without conflicts or retries.
- Individual list items or map/object elements can be owned and updated by different actors.
- The object does not need to be read to construct a patch--it is enough to start with an object in its blank (default) state and apply necessary changes. The patch can then be submitted and per-field concurrency checks will be applied by the server.

> Another mechanism for granular object updates is/was a "strategic merge patch". It can still be found in `controller-runtime` APIs, but it could only be used with built-in Kubernetes object kinds, and is considered obsolete.

### Choosing the object method update
In general, there are no hard rules for choosing the API to update objects, but here are some thoughts to consider:
- Patching is often preferable to updating because it is less error-prone and reduces the amount of network traffic. Updating involves sending all the object data, and increases the possibility of changing something inadvertently.
- Updates are always subject to optimistic concurrency, but with patching the controller has a choice.
- A JSON merge patch with optimistic locking is a good compromise and a safe default.
- Server-side apply patch has been designed for the scenario when multiple actors update different portions of object data.
- When a change that involves most/all object data, a full update might make sense.


## Reacting to external (real-world) changes

In addition to handling changes to Kubernetes objects (their spec in particular), controllers often need to react to changes to real-world entities that their Kubernetes objects represent. `controller-runtime` has a mechanism that helps with that: the *channel-based event source*. The way it works is, whenever a `GenericEvent` structure is written to a special channel, the `controller-runtime` will schedule a reconciliation function run. Here is how it can be set up:

```go
type XxxReconciler struct {
  ctrl_client.Client // "sigs.k8s.io/controller-runtime/pkg/client"
  // ... other reconciler data

  // The channel that will trigger reconciliation
  changeNotify chan ctrl_event.GenericEvent // "sigs.k8s.io/controller-runtime/pkg/event"
}

func NewXxxReconciler(...) *XxxReconciler {
  r := XxxReconciler {
    //... reconciler data initialization

    // allocate change notification channel
    r.changeNotify: make(chan ctrl_event.GenericEvent)
  }
}

func (r *XxxReconciler) SetupWithManager(mgr ctrl.Manager) error {
  src := ctrl_source.Channel{
    Source: r.notifyProcessChanged,
  }

  return ctrl.NewControllerManagedBy(mgr).
    For(&XxxObjectKind{}).

    // Tell controller_runtime to invoke the reconciliation function whenever an event is written to the channel
    Watches(&src, &ctrl_handler.EnqueueRequestForObject{}). // "sigs.k8s.io/controller-runtime/pkg/handler"

    Complete(r)
}
```

The `GenericEvent` instance needs to include the Kubernetes object name and namespace (`NamespacedName`) that will be a parameter to reconciliation function. It is up to the controller to make sure that it keeps enough context to know which Kubernetes object should be associated with every real-world entity it is watching. Most often that information is stored in the Kubernetes object status.

The controller also needs to take care that it does not trigger too many reconciliation invocations. If real-world is changing rapidly, it can be beneficial to batch or debounce reconciliations for corresponding Kubernetes objects. DCP has  [`resiliency` package](https://github.com/microsoft/usvc-apiserver/tree/main/internal/resiliency) that can help with that.

## Deleting Kubernetes objects

In the simplest case, when a client deletes a Kubernetes object, that object immediately disappears from the Kubernetes data store. The controller (if any) will receive a change notification and its reconciliation function will be run, but an attempt to `Get()` the object will result in `NotFound` error. The object data is gone. If the controller needs to do any cleanup work, all it has is the deleted object name and namespace. In case this is insufficient, the controller needs to employ a finalizer.

### Finalizers
A finalizer is nothing more than a tag that is added to a list (called `finalizers`) that is part of Kubernetes object metadata. This tag is typically added by a controller upon object creation and signifies, that the controller that added the finalizer needs to do some work before the object data is completely erased from the data store. Here is how it works:

1. When a delete request for an object arrives, Kubernetes API server will check the object finalizer list. If it is not empty, the API server will not delete the object immediately. Instead, it will set the object's `DeletionTimestamp` property to current system time. This triggers a change notification for all controllers watching the object.

1. When a controller sees an object with non-zero `DeletionTimestamp`, it knows the object is in the process of being deleted. The controller's role is to perform whatever cleanup work is necessary, and then **remove its finalizer (tag)** from the `finalizers` list.

1. The Kubernetes will be watching the object as well. When all controllers are done with the cleanup work and the `finalizers` list becomes empty, Kubernetes will erase the object from the data store, finishing the deletion process.

You can read more about how finalizers work in [Kubernetes documentation](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/).

### Deleting object hierarchies

Kubernetes objects can form a hierarchy (actually, a directed graph) via `ownerReferences` metadata property. Objects can have multiple owners. The general rule is, when ALL owners are deleted, the object with no owners left is a candidate for automatic deletion. The following discussion assumes a single owner; when multiple owners are present, deleting the owner does not affect children other than the need to update `ownerReferences` (by removing the reference to deleted owner).

Kubernetes does not implement a true auto-deletion of orphaned children. The deletion is always facilitated by some controller; however, Kubernetes has ability to specify the intent for how children should be deleted as part of parent deletion request. This is known as "propagation policy". Kubernetes recognizes three different policies:

- `Background` (the default): the owner deletion is completed first; its children are deleted afterwards, asychronously. This means any client looking for the owner object will see the owner and all its childern, or receive a `NotFound` error.
- `Foreground`: with this policy, children that have an `ownerReference` with `blockOwnerDeletion` flag set to true must be deleted first, before the owner is garbage-collected. For clients it means that they may observe the owner in a "being deleted" state, with some children deleted, but others not (yet).
- `Orphan`: this policy effectively ignores the owner references, decoupling the lifetime of the owner from its children.

For more information refer to [Garbage Collection topic](https://kubernetes.io/docs/concepts/architecture/garbage-collection/) in Kubernetes documentation and [Using Finalizers to Control Deletion](https://kubernetes.io/blog/2021/05/14/using-finalizers-to-control-deletion/) Kubernetes blog post.

The presence of finalizers can make the orchestration of object hierarchy deletion quite complicated. The most straightforward tactics is often to limit the scope of auto-deletion by relying on finalizers and owner object controllers to delete children as appropriate. This works well if it is the parent object controller that creates the children (think ReplicaSet and its Replicas).

Another possibility for object hierarchy deletion is to have child controllers watch the parent objects and delete children when the parent gets removed. This option is useful if it is the parent controller is not involved in child creation.
