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


## Making DCP available from $PATH

### macOS, Linux, WSL
To make `dcp` CLI available from command line on non-Windows system, run `sudo -E make link-dcp`. This is a one-time operation that will create a link from `/usr/local/bin/dcp` to `~/.dcp/dcp` executable. Not recommended for machines shared by many people :-) but handy for a development box.

### Windows
On Windows open the Environment Variables applet and add `$USERPROFILE\.dcp` to PATH.


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


## Troubleshooting and debugging tips

### `make lint` times out (or ends with an error that says "Killed") 
We have seen the linter occasionally go into a persistent, bad state. Do `make clean`, then retry `make lint` again.

### Make it easier to use `kubectl` with DCP 
Define a shell alias.

Linux/macOS:

```shell
alias kk='kubectl --kubeconfig ~/.dcp/kubeconfig
```

Windows:

```powershell
function dcpKubectl() { & kubectl --kubeconfig "$env:USERPROFILE\.dcp\kubeconfig" $args }
Set-Alias kk dcpKubectl
``` 

For working with DCP in the context of Aspire tooling (which creates a separate `kubeconfig` file for every application run) the following PowerShell function/alias might be useful:

```powershell
function dcpdKubectl() {
    $DcpPid = $null
    if ($args.Length -gt 1 -and $args[0] -eq "-DcpPid") {
        $DcpPid = $args[1]
        $args = $args | Select-Object -Skip 2
    }

    if ($null -ne $DcpPid) {
        $dcpProcess = Get-Process -Id $DcpPid
        if ($? -eq $false) {
            throw "No DCP process with PID $DcpPid found"
        }

        $kubeconfig = $dcpProcess | Select-Object -ExpandProperty CommandLine | ForEach-Object { $_.Split() } | Where-Object { $_.EndsWith("kubeconfig") -and ($_ -ne "--kubeconfig") }
    } else {
        $dcpProcesses = @(Get-Process | Where-Object { $_.Name -ceq "dcpd" })
        if ($dcpProcesses.Count -eq 0) {
            throw "No DCP processes found"
        } elseif ($dcpProcesses.Count -gt 1) {
            Write-Error "Multiple DCP processes found, use -DcpPid parameter to specify which one to use (pass DCP process ID)"
            $dcpProcesses | Select-Object Id, CommandLine | Format-List
            return
        } else {
            $kubeconfig = $dcpProcesses[0].CommandLine.Split() | Where-Object { $_.EndsWith("kubeconfig") -and ($_ -ne "--kubeconfig") }
        }
    }

    & kubectl --kubeconfig "$kubeconfig" $args
}

Set-Alias kk -Value dcpdKubectl
```

You invoke it with the name of the Aspire app host project (the one that is set as startup project), for example `kkm AppHost api-resources`.

> For debugging Aspire tests (part of `CloudApplicationTests` suite) the name of the relevant process that started DCP is `testhost`.
 
### After `make generate-openapi` the generated file is empty (almost all contents has been removed).
Looks like the OpenAPI code generator failed. Run `make generate-openapi-debug` to enable debug output and check if it contains any clues. 

We have seen an issue where the generator would latch to a specific version of `go` compiler and and fail when the compiler is updated. Deleting `.toolbin/openapi-gen` binary usually helps in this case.


### I need to test a local build of `dcp` with Aspire tooling

Add the following snippet to your AppHost project:

```xml
    <PropertyGroup>
      <DcpDir>[folder where dcp.exe lives]</DcpDir>
    </PropertyGroup>
```

Just be careful about not checking in this change and remember to comment out the `<DcpDir></DcpDir>` line if you want to go back to using the DCP version Aspire targets.

Alternatively, you can create a file named `AppHost.csproj.user` next to the `AppHost.csproj` file, with the following content:

```xml
    <Project>
        <PropertyGroup>
            <DcpDir>[folder where dcp.exe lives]</DcpDir>
        </PropertyGroup>
    </Project>
```

### Need to get detailed logs from DCP run

Set the `DCP_DIAGNOSTICS_LOG_LEVEL` environment variable to `debug`. The logs will be put to `${TEMP}/dcp/logs`, although you can change the destination by setting `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable.

### I need to debug DCP

There are several VS Code debug configurations available for this repository. They are very straightforward; take a look at `launch.json` and choose one that fits your needs best, or create a custom one for your scenario. 

If you need to learn morea about Go debugging in VS Code, [VS Code Go debugging wiki](https://github.com/golang/vscode-go/wiki/debugging) and [documentation for the underlying delve Go debugger](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) might be helpful.

> Note: you want to use `make compile-debug` for building DCP for debugging. By default DCP is built with optimizations on, which can result in strange behavior during debugging (somewhat unpredictable order of statements, local data "disappearing" in the middle of a function etc.)

### I need to debug DCP in the context of Aspire (Visual Studio-based) run

The following procedure can be used to debug DCP controllers when an application is run from Visual Studio:

1. Open the solution with your application in Visual Studio. 
1. Set a breakpoint in `ApplicationExecutor.RunApplicationAsync` method. The class is in `Aspire.Hosting.Dcp` namespace (a "Function Breakpoint" that refers to the fully-qualified name of the method will work fine, you do not need to have `Aspire.Hosting` project in your solution).
1. Open `usvc-apiserver` repository in Visual Studio Code. 
1. Run the application. When the breakpoint is hit, the DCP API server and controller host should already be started, but no workload objects have been created yet. 
1. Switch to Visual Studio Code, select `attach to controller process` debug configuration and start debugging. When prompted, select the `dcpctrl` process (there should be just one). Set breakpoints in controller code as necessary. 
1. Switch back to Visual Studio and continue (F5). The workload definition will be created by the `ApplicationExecutor` and sent to DCP for execution.

The same steps can also be used to debug `ApplicationExecutor` (in Visual Studio) if you suspect that the workload that DCP receives is set up incorrectly.


### Taking performance traces

See [performance investigations page](performance-investigations.md).
