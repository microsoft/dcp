# Service discovery in DCP

Services in a multi-service applications need a mechanism to find out network addresses (including ports) of their dependencies. This is referred to as service discovery. DCP implements a simple service discovery mechanism based on environment variables.

## Services and Endpoints

To facilitate service discovery, DCP provides built-in `Service` and `Endpoint` objects.

### `Service` objects

A `Service` instance represents a network endpoint with a specific contract. Clients can use `EffectiveAddress` and `EffectivePort` properties of a `Service` instance to learn the service address and make calls to it. 

The service contract (set of operations that are available) is not modeled in DCP; the rule of thumb is to use a separate `Service` object for any endpoint that requires different network address (more precisely, address + port combination). For example, if a .NET Aspire web API service "Catalog" exposes both HTTP and HTTPS endpoints, each of them would be modeled as a separate `Service` instance in DCP model (e.g. named "Catalog_HTTP" and "Catalog_HTTPS"). If the same Catalog web API exposes a gRPC endpoint for metric scraping, that would result in yet another `Service` instance (e.g. named "Catalog_metrics").

### `Endpoint` objects

An `Endpoint` instance ties a `Service` to an implementation: it represents an instance of a program that can handle a network request. It contains a `ServiceName` property that points to the `Service` the `Endpoint` implements, and `Address` and `Port` properties tell clients how to reach it.

The "program that can handle a network request" can be part of DCP model (an `Executable` or a `Container` object), or it can be an external service: an on-premises database server or a cloud service. In the former case the `Executable` or `Container` instance becomes an "owner" of the `Endpoint`: its name is stored in the `OwnerReferences` metadata property. An `Executable` or `Container` can (an often does) implement multiple `Services`, thus an `Executable` or `Container` instance can "own" multiple `Endpoints`. DCP ensures that when the owner is deleted, its `Endpoints` are gone too. 

A `Service` can have more than one `Endpoint`; it can also have none. If a `Service` has at least one `Endpoint`, it is considered in a "ready" state (ready to serve traffic), otherwise it is "not ready". This is reflected in the `State` property of the `Service` object. 

#### Why have `Endpoints` at all?
One might ask why there is a separation between `Service` and `Endpoint` types; why not have just one, e.g. `Service`? The main reason is **it allows clients to have stable addresses to work with**: 

- DCP will proxy calls from each `Service` to one of the corresponding `Endpoints`, and will dynamically change the proxy configuration depending on how many `Endpoints` are available. 
- The `Service` `EffectiveAddress` and `EffectivePort` are available shortly after `Service` object is created, they never change for the lifetime of the `Service`, and there is no requirement for any `Endpoints` to exist at the time of `Service` creation.

This means that as long as clients of a `Service` are created after the `Service` gets its `EffectiveAddres`/`EffectivePort` combination, the clients can get an address for `Service` that never changes, regardless what how many `Endpoints` the `Service` has at any point in time. 

## `Endpoint` creation
`Endpoint` instances are created whenever a program or container implementing a particular `Service` becomes ready to serve requests. `Services` representing things that are provisioned outside of DCP (cloud services, on-premises database servers etc.) are usually created together with their `Endpoints`. 

> Note: DCP version 0.1.42 (.NET Aspire Preview 1) does not have any support for `Endpoint` authentication. Obtaining correct credentials for a `Service` and attaching these credentials to requests is something that each `Service` client must do on its own.

The same explicit `Endpoint` creation can be done for `Executables` and `Containers` that are part of the local application workload, but for those kinds of objects DCP provides a more convenient and powerful `Endpoint` creation mechanism: `service-producer` annotation. 

### `service-producer` annotation

When applied to `Executable` or `Container`, the annotation indicates that the object implements ("produces") a `Service`, which makes DCP create an `Endpoint` for it automatically. 

> In the discussion below, the term "instance" means "`Executable` or `Container` that has a `service-producer` annotation applied to it".

The `service-producer` annotation format is an array of JSON objects. Each object corresponds to a different `Service` produced by an instance and has the following properties:

| Property | Description |
| --- | --------- |
| `serviceName` | Name of the `Service` that the instance implements. Mandatory. |
| `address` | The address used by the instance to serve the `Service`. <br/><br/> For containers this is the address that the workload should listen on INSIDE the container. This is NOT the address that the container will be available on the host network; that address is part of the `Container` spec, specifically it is the `HostIP` property of the `ContainerPort` definition(s). <br/><br/> Optional, defaults to `localhost` if not present. |
| `port` | Port used by the instance to serve the service. <br/> <br/> For `Containers` it is mandatory and must match one of the `Container` ports. We first match on `Port.HostPort`, and if one is found, we use that port. If no matching `HostPort` is found, we match on `Port.ContainerPort`. If such port is found, we proxy to the corresponding host port (either specified explicitly, or auto-allocated by container orchestrator). <br/> <br/> For Executables the port is also required *unless* the Executable also expects the port to be injected into it via environment variable and template `portForServing` function (see below). |

### `portForServing` template function

The `portForServing` template function can be used to make DCP auto-allocate a port for an `Executable`. That port can then be injected into an `Executable` via an environment variable (`env` part of the Spec), or via a startup parameter (`args` part of the Spec). 

Consider the following example, assuming a `Service` named "Catalog_HTTP" has already been created:

```yaml
apiVersion: usvc-dev.developer.microsoft.com/v1
kind: Executable
metadata:
  name: Catalog
  annotations:
    service-producer: "[{\"serviceName\": \"Catalog_HTTP\"}]"
spec:
  executablePath: "/home/joe/code/myapp/src/catalog/bin/debug/catalog"
  env:
    - name: "CATALOG_SVC_PORT"
      value: "{{- portForServing \"Catalog_HTTP\" -}}"
```

When this `Executable` object is created, DCP will:

1. Recognize `service-producer` annotation on the Catalog `Executable` and infer from the annotation that Catalog program instances produce Catalog_HTTP service. 
1. Treat the value of the `CATALOG_SVC_PORT` environment variable in the spec as a template, replacing it with a port that is allocated by DCP for the purpose of **the Catalog program instance serving the Catalog_HTTP service**. The Catalog program instance is expected to bind to that port for serving requests.
1. After the Catalog program instance is started, DCP creates an `Endpoint` for it (with `ServiceName` property equal to "Catalog_HTTP"). This makes the proxy for Catalog service forward all requests to this Catalog program instance and transitions the Catalog_HTTP service to "ready" state.

> The syntax used by DCP for environment value templates is [Go templates](https://pkg.go.dev/text/template). The double curly braces `{{ }}` indicate the template boundaries. The dash sign adjacent to the curly braces (`{{-` and `-}}`) makes the template system discard the leading/trailing whitespace, respectively.
>
> The template context is the DCP `Executable` object that uses the template. Thus any property of the object that is available at the time of program creation can be used to populate environment variables. For example, `{{- .Name -}}` will set the environment variable to the name of the `Executable`. 

The use of `service-producer` annotation and `portForServing` template function is particularly useful for replicated services (the ones created via `ExecutableReplicaSet`). Each replica needs a unique port, which must be dynamically allocated; the port cannot be specified statically in the `ExecutableReplicaSet` spec. As the replica set is scaled up or down, DCP allocates ports, and creates and deletes `Endpoints` for each replica automatically. The inbound traffic uses all available `Endpoints` in a round-robin fashion.

> For replicated services the `service-producer` annotation is applied to the `ExecutableReplicaSet`. The replica set then applies the annotation automatically, unchanged, to each replica (`Executable` instance) it creates.

> Assuming multiple `Endpoints` exist, new `Endpoint` will be used for a request if a client uses a new connection. If the client keeps the connection open for several requests, new `Endpoints` may or may not be used depending on how close, time-wise, the requests are made, and on `Endpoint` connection keep-alive policy. 

### `addressForServing` template function

If a non-default address is used for producing a service (i.e. not `localhost`), the instance can be passed that address via `addressForServing`. The function verifies that the instance has appropriate `service-producer` annotation with the matching name and then extracts the `address` property out of it. This information, passed to the instance via environment variable or a startup parameter, can then be used inside instance code to bind to correct IP address for the service.

## `Service` consumption (`portFor` and `addressFor` template functions)

If a `Service` uses a well-known (static) port (via `Port` spec property), DCP will try to make the `Service` available for clients on that port. This may not work if the port is already taken by some other program running on the machine before DCP workload is started.

A more robust option is for DCP to auto-allocate the port for a `Service` (by leaving the `Port` property of the `Service` object set to its default value, i.e. zero). Clients can learn the port for the `Service` using `portFor` template function. For example, an instance that needs to call the Catalog service, could get the `CATALOG_SVC_PORT` environment variable set to `{{- portFor "Catalog_HTTP" -}}` and use the injected value to connect to the Catalog_HTTP service.

The injected port is the `EffectivePort` of the `Service`. This ensures that the clients have a stable port to work with, regardless how many replicas of the program(s) implementing the `Service` are running at any given time.

If the `Service` is using a non-standard address (i.e. not `localhost`), clients can learn what the address is via `addressFor` template function. It is used in the same way as `portFor` template function, but returns the `Service` address instead.

 