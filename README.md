# DCP API server and DCP CLI
This repository contains core components of Developer Control Plane tool:
-  `dcp` CLI that users invoke to run applications and for other, related tasks.
-  `dcpd` is the DCP API server that holds the workload model. It is used by controllers, workload renderers, and API providers to create workload definition, run it, and expose information about it.

	`dcpd` is Kubernetes-compatible. It is implemented using [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.


## Development environment setup
You will need:
- Go 1.19 or newer
- `make` tool (`make` version 3.81 or newer)

Supported operating systems for development are Linux and MacOS; Windows is not supported at this time.

### Go module system setup
Until `usvc-dev` project becomes public (no plans for that currently), the Go module system needs to be told that repositories under this project are private and global proxies/checksums should not be used for them:

```shell
go env -w 'GOPRIVATE=github.com/usvc-dev/*'
```

This setting applies to your Go installation (is shared between all repositories cloned onto your development machine). For more information see [Go private modules documentation](https://go.dev/ref/mod#private-modules).

### GitHub authentication setup
If you are using SSH to authenticate to GitHub, you want the following in your `~/.gitconfig` file:

```shell
[url "ssh://git@github.com/"]
	insteadOf = https://github.com/
```

 For more information see [Go configuration for SSH Git authentication](https://go.dev/doc/faq#git_https).


## Running `dcpd`

To start DCP API server run

```shell
make run-dcpd
```

To connect to the API server using `kubectl` and similar Kubernetes tools use the kubeconfig file in the root of this repository. For example (the command lists available API resources):

```shell
kubectl --kubeconfig ./kubeconfig api-resources
```

To shut down the DCP API server just press Ctrl+c in the terminal.


## Debugging `dcpd`

A debugging configuration named `dcpd launch` is provided to run dcpd under the debugger. You can F5 it in vs code normally, nothing extra is required other than VS Code Go extension. You might want to change the current working directory though, so that relative paths to executables are resolved properly.

If you want to debug one of the controllers, the code is under `${GOPATH}/pkg/mod/github.com/usvc-dev/stdtypes@vX.Y.Z/controllers`, where `vX.Y.X` is the current version of the `stdtypes` package that this repository consumes (check `go.mod` file in the root of the repository).
