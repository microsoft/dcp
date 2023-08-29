# DCP monorepo
This repository contains the core components of the Developer Control Plane tool:
-  `dcp` CLI that users invoke to run applications and for other, related tasks.
-  `dcpd` is the DCP API server that holds the workload model. It is used by controllers, workload renderers, and API providers to create workload definition, run it, and expose information about it.

	`dcpd` is Kubernetes-compatible. It is implemented using [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.
-  `dcpctrl` is the core DCP controllers that implement the standard behavior for DCP workload models.


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


## Running tests

### Run all tests
`make test` will install all dependencies and run all the tests.

### Running subsets of tests from command line
You can also run individual tests from command line, including integration tests. To do so you just need to ensure that the K8s binaries are downloaded and set the `KUBEBUILDER_ASSETS` environment variable. To see what the variable value should be (and install K8s test binaries as a side effect) do

```shell
make show-test-vars
```
After that set the `KUBEBUILDER_ASSETS` environment variable, for example

```shell
$env:KUBEBUILDER_ASSETS = '(value shown by make show-test-vars)' # Windows
export KUBEBUILDER_ASSETS='(value shown by make show-test-vars)' # Non-Windows
```
Now you can run `go test` commands to run tests, for example, to run just Endpoint controller tests in verbose mode with race detection:

```shell
 go test -race -run TestEndpoint -v ./test/integration/...
```

### Debugging tests
You can also run individual tests via VS Code "run test" and "debug test" gestures. A few caveats:

1. Run `make test` at least once from command line before debugging tests. This will ensure that test K8s binaries are downloaded and installed properly into `.toolbin` directory.
1. Integration test timeouts are increased to 60 minutes (refer to `.vscode/settings.json` to change that). This helps with test debugging.
1. If the test is killed while running under the debugger, it will leave orphaned `kube-apiserver` and `etcd` processes. You can check if such processes exist by running following commands: 

    | Task | Command (macOS) | Command (Linux) | Command (Windows) |
    | --- | --- | --- | --- |
    | Check for orphaned `kube-apiserver` and `etcd` processes. | `pgrep -lf kube-apiserver` <br/> `pgrep -lf etcd` | `pgrep -af kube-apiserver` <br/> `pgrep -af etcd` | `pslist kube-apiserver` <br/> `pslist etcd` |
    | Kill orphaned `kube-apiserver` and `etcd` processes. | `pkill -lf kube-apiserver` <br/> `pkill -lf etcd` | `pkill -af kube-apiserver` <br/> `pkill -af etcd` | `pskill kube-apiserver` <br/> `pskill etcd` |

    `pslist` and `pskill` Windows tools are part of [Sysinternals tool suite](https://learn.microsoft.com/sysinternals/).


## Troubleshooting tips

| Issue | Tip |
| --- | ------- |
| `make lint` times out (or ends with an error that says "Killed") | We have seen the linter occasionally go into a persistent, bad state. Do `make clean`, then retry `make lint` again. |
| Make it easier to use `kubectl` with DCP | Define a shell alias:Linux/macOS: <br/> &nbsp; &nbsp; `alias kk='kubectl --kubeconfig ~/.dcp/kubeconfig'` <br/> Windows: <br/> &nbsp; &nbsp; `function dcpKubectl() { & kubectl --kubeconfig "$env:USERPROFILE\.dcp\kubeconfig" $args }` <br/> &nbsp; &nbsp; `Set-Alias kk dcpKubectl` |

