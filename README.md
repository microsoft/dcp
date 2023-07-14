# DCP API server and DCP CLI
This repository contains core components of Developer Control Plane tool:
-  `dcp` CLI that users invoke to run applications and for other, related tasks.
-  `dcpd` is the DCP API server that holds the workload model. It is used by controllers, workload renderers, and API providers to create workload definition, run it, and expose information about it.

	`dcpd` is Kubernetes-compatible. It is implemented using [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.


## Development environment setup
You will need:
- Go 1.20 or newer
- `make` tool (`make` version 3.81 or newer; `make` 4.4.0 or newer is recommended)

Supported operating systems for development are Linux, MacOS, and Windows. On Windows, in addition to the `make` tool, you will need the following command-line tools to be installed and on `PATH`:
- awk (tested with GNU awk 5.2.2)
- curl (tested with curl 8.0.1)
- golangci-lint (tested with golangci-lint 1.53.3)


### Go module system setup
Until DCP project becomes public (no plans for that currently), the Go module system needs to be told that repositories under this project are private and global proxies/checksums should not be used for them:

```shell
go env -w 'GOPRIVATE=github.com/microsoft/usvc-*'
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
make build
make install
~/.dcp/ext/dcpd -v=debug
```

To connect to the API server using `kubectl` and similar Kubernetes tools use the kubeconfig file in the root of this repository. For example (the command lists available API resources):

```shell
kubectl --kubeconfig ~/.dcp/kubeconfig
```

To shut down the DCP API server just press Ctrl+c in the terminal.


## Debugging `dcpd`

A debugging configuration named `dcpd launch` is provided to run dcpd under the debugger. You can F5 it in vs code normally, nothing extra is required other than VS Code Go extension. You might want to change the current working directory though, so that relative paths to executables are resolved properly.

## Making DCP available from $PATH

### macOS, Linux, WSL
To make `dcp` CLI available from command line on non-Windows system, run `sudo -E make link-dcp`. This is a one-time operation that will create a link from `/usr/local/bin/dcp` to `~/.dcp/dcp` executable. Not recommended for machines shared by many people :-) but handy for a development box.

### Windows
On Windows open the Environment Variables applet and add `$USERPROFILE\.dcp` to PATH.

## Debugging DCP CLI

A couple of debugging configurations are provided in `.vscode/launch.json`:

- `dcp start-apiserver` will run the API server and controllers
- `dcp up todo-csharp-sql-swa-func` will do `dcp up` on the ToDo CSharp SQL Azure sample application.

You can use these configurations as a starting point to create your own configurations for specific scenarios. Check out [VS Code Go debugging wiki](https://github.com/golang/vscode-go/wiki/debugging) for more information.

## Troubleshooting tips

| Issue | Tip |
| --- | ------- |
| `make lint` times out (or ends with an error that says "Killed") | We have seen the linter occasionally go into a persistent, bad state. Do `make clean`, then retry `make lint` again. |
| Make it easier to use `kubectl` with DCP | Define a shell alias:Linux/macOS: <br/> &nbsp; &nbsp; `alias kk='kubectl --kubeconfig ~/.dcp/kubeconfig'` <br/> Windows: <br/> &nbsp; &nbsp; `function dcpKubectl() { & kubectl --kubeconfig "$env:USERPROFILE\.dcp\kubeconfig" $args }` <br/> &nbsp; &nbsp; `Set-Alias kk dcpKubectl` |

