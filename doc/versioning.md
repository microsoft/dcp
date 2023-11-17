# DCP versioning

This document decribes what promises DCP makes with regards to backward compatibility and how clients can verify that the version of DCP they are working with.

## API versioning

DCP APIs are organized into groups and versions, following the Kubernetes model. Once a particular group-version API combination is released, DCP will maintain a high degree of compatiblity for that group-version combination, equivalent to [Kubernetes stable API backward compatibility](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api_changes.md#on-compatibility). More specifically, multiple releases of DCP will not change the semantics of existing API version, including:

- The effects of using existing API types, fields, values, well-known annotations, and environment variable template functions.
- Which fields are required and which are not.
- Which fields are mutable and which are not.
- The domain (set of valid values) for each field.

DCP makes no compatibility guarantess for APIs that belong to different versions.

### Compatible changes for stable APIs

Incremental changes that satisfy the criteria above are considered backward-compatible. Some examples of backward-compatible changes include:

- Adding a new, optional field to an API type.
- Adding a new, well-known annotation that changes the behavior associated with an API type.
- Adding a new environment variable template function.
- Adding a new API type.

Clients must be prepared that new DCP release might include new (optional) fields for existing API types. When processing DCP data the **fields that client does not "know about" should be ignored**. 

Also, when updating existing API objects, **the client should always use patch update strategy** (mutate specific fields only) to ensure that fields the client does not understand are not accidentally reset to their default values.

## Checking DCP version and supported API versions

### Checking DCP version

DCP clients can verify the version of DCP by issuing `dcp version` CLI command. This command returns DCP CLI version information in JSON format:

```powershell
> & 'C:\Program Files\dotnet\packs\Aspire.Hosting.Orchestration.win-x64\8.0.0-preview.1.23530.4\tools\dcp.exe' version
{"version":"0.1.40","commitHash":"baecaa497c5244f7c940b94659ba433d371f0bac","buildTimestamp":"2023-10-25T14:48:21-07:00"}
```

> The default DCP CLI version (applied when DCP is built outside of CI/CD pipeline) is `dev`. That version should always be considered "current".

### Supported API types and their versions

DCP supports the standard Kubernetes metadata `/apis` path. When queried, this path will return the list of all API types (kinds) supported by DCP instance, with full version information and supported verbs. In the C# Kubernetes client this call corresponds to `GetAPIVersionsWithHttpMessagesAsync` method. Here is an example response obtained from this path:

```jsonc
{
    "kind": "APIGroupDiscoveryList",
    "apiVersion": "apidiscovery.k8s.io/v2beta1",
    "metadata": {},
    "items": [
        {
            "metadata": {
                "name": "usvc-dev.developer.microsoft.com",
                "creationTimestamp": null
            },
            "versions": [
                {
                    "version": "v1",
                    "resources": [
                        {
                            "resource": "containers",
                            "responseKind": { "group": "", "version": "", "kind": "Container" },
                            "scope": "Cluster",
                            "singularResource": "",
                            "verbs": [ "create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"],
                            "shortNames": [  "ctr" ],
                            "subresources": [
                                {
                                    "subresource": "status",
                                    "responseKind": { "group": "", "version": "", "kind": "Container" },
                                    "verbs": [ "get", "patch", "update" ]
                                }
                            ]
                        },
                        {
                            "resource": "executables",
                            "responseKind": { "group": "", "version": "", "kind": "Executable" },
                            "scope": "Cluster",
                            "singularResource": "",
                            "verbs": [ "create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"],
                            "shortNames": [ "exe" ],
                            "subresources": [
                                {
                                    "subresource": "status",
                                    "responseKind": { "group": "", "version": "", "kind": "Executable" },
                                    "verbs": [ "get", "patch", "update" ]
                                }
                            ]
                        }
                        // (other DCP API types omitted for brevity)
                    ]
                }
            ]
        }
    ]
}
```


## Appendix: supported API versions for major DCP releases

| Release | Supported API groups and versions |
| ------ | ------ |
| `0.1.42` (Aspire preview 1 release) | `usvc-dev.developer.microsoft.com/v1` |
