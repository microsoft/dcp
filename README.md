# DCP monorepo
This repository contains the core components of the Developer Control Plane tool:
-  `dcp` CLI that users invoke to run applications and for other, related tasks. If invoked with `start-apiserver` it will act as the API server that holds the workload model used by controllers, workload renderers, and API providers to create a workload definition, run it, and expose information about it. The API server is Kubernetes-compatible. It is implemented using [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.
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
Now you can run `go test` commands to run selected tests, including integration tests. for example, to run just Endpoint controller tests in verbose mode with race detection:

```shell
 go test -race -count 1 -run TestEndpoint -v ./test/integration/...
```
`-count 1` is a Go idiom that forces test (re-)run even if Go thinks nothing has changed and a previous, cached test result is still valid. `-race` enables automatic race detection. which is always worth enabling, but is unfortunately not supported on Windows (but is supported inside WSL). For more information on these, and other Go test options run `go help testflag`.

`TEST_CONTEXT_TIMEOUT` is an environment variable that can be set to change the default test timeout (60 seconds). E.g. `TEST_CONTEXT_TIMEOUT=30` changes it to 30 seconds.

### Debugging tests
You can also run individual tests via VS Code "run test" and "debug test" gestures. When tests are running under the debugger, the timeout is increased to 60 minutes via VS Code repository settings.

If the test is killed while running under the debugger, may leave orphaned `dcp` process. You can check if such processes exist by running following commands:

| Task | Command (macOS) | Command (Linux) | Command (Windows) |
| --- | --- | --- | --- |
| Check for orphaned `dcp` processes. | `pgrep -lf dcp` | `pgrep -af dcp` | `pslist dcp` |
| Kill orphaned `dcp` processes. | `pkill -lf dcp` | `pkill -af dcp` | `pskill dcp` |

`pslist` and `pskill` Windows tools are part of [Sysinternals tool suite](https://learn.microsoft.com/sysinternals/).


## Troubleshooting and debugging tips

### `make lint` times out (or ends with an error that says "Killed")
We have seen the linter occasionally go into a persistent, bad state. Do `make clean`, then retry `make lint` again.

### Make it easier to use `kubectl` with DCP
For working with DCP in the context of Aspire tooling (which creates a separate `kubeconfig` file for every application run) the following set of PowerShell functions and aliases might be useful:

```powershell
function dcpKubeconfigPath() {
    param (
        [string]$dcpPid
    )

    if (-not [string]::IsNullOrWhiteSpace($dcpPid)) {
        $dcpProcess = Get-Process -Id $dcpPid

        if ($? -eq $false) {
            throw "No DCP process with PID $dcpPid found"
        }

    } else {
        # Use "dcp" process to figure out where the kubeconfig file is --
        # when debugging the API server we might be running without a controllers process (dcpctrl).
        $dcpProcesses = @(Get-Process | Where-Object { $_.Name -ceq "dcp" })
        if ($dcpProcesses.Count -eq 0) {
            throw "No DCP processes found"
        } elseif ($dcpProcesses.Count -gt 1) {
            $dcpProcesses | Select-Object Id, CommandLine | Format-List
            throw "Multiple DCP processes found, use -DcpPid parameter to specify which one to use (pass DCP process ID)"
        } else {
            $dcpProcess = $dcpProcesses[0]
        }
    }

    $kubeconfig = $dcpProcess | Select-Object -ExpandProperty CommandLine | ForEach-Object { $_.Split() } | Where-Object { $_.EndsWith("kubeconfig") -and ($_ -ne "--kubeconfig") }
    if ([string]::IsNullOrWhiteSpace($kubeconfig)) {
        $kubeconfig = "$env:USERPROFILE/.dcp/kubeconfig"
    }

    return $kubeconfig
}

function dcpKubeconfigContent() {
    $kubeconfig = dcpKubeconfigPath
    Get-Content $kubeconfig
}

function dcpKubectl() {
    $dcpPid = $null
    if ($args.Length -gt 1 -and $args[0] -eq "-DcpPid") {
        $dcpPid = $args[1]
        $args = $args | Select-Object -Skip 2
    }

    $kubeconfig = dcpKubeconfigPath $dcpPid

    & kubectl --kubeconfig "$kubeconfig" $args
}

Set-Alias kk dcpKubectl
Set-Alias kkconfig dcpKubeconfigContent
```

To issue a command against the DCP API server use `kk` alias. Fore example, to display a list of running Executable objects do `kk get exe`.

`kkconfig` will display the content of the API server config file that is used by the running Aspire app. This can be useful to find out which port the API server is running at.

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

### I need to debug DCP controllers in the context of an Aspire (Visual Studio-based) application run

The following procedure can be used to debug DCP controllers when an application is run from Visual Studio:

1. Open the solution with your application in Visual Studio.
1. See the section above for "I need to test a local build of `dcp` with Aspire tooling".
1. Set a breakpoint in `ApplicationExecutor.RunApplicationAsync` method. The class is in `Aspire.Hosting.Dcp` namespace (a "Function Breakpoint" that refers to the fully-qualified name of the method will work fine, you do not need to have `Aspire.Hosting` project in your solution).
1. Open `usvc-apiserver` repository in Visual Studio Code.
1. Run the application. When the breakpoint is hit, the DCP API server and controller host should already be started, but no workload objects have been created yet.
1. Switch to Visual Studio Code, select `attach to controllers process` debug configuration and start debugging. When prompted, select the `dcpctrl` process (there should be just one). Set breakpoints in controller code as necessary.
1. Switch back to Visual Studio and continue (F5). The workload definition will be created by the `ApplicationExecutor` and sent to DCP for execution.

The same steps can also be used to:
- Debug `ApplicationExecutor` (in Visual Studio) if you suspect that the workload that DCP receives is set up incorrectly.
- Debug code that is hosted by the DCP API server (e.g. log storage/streaming), except that you want to attach to the API server process instead, using `attach to API server process` debug configuration.

> Tip: if the API server request is failing before hitting DCP code, a good place to put a breakpoint is one of the endpoint handlers in `${GOPATH}/pkg/mod/k8s.io/apiserver@{version}/pkg/endpoints/handlers`. You can figure out which `apiserver` package version DCP is using from the `go.mod` file, and your `${GOPATH}` value can be learned by running `go env` command.

### Taking performance traces

See [performance investigations page](performance-investigations.md).

### Environment variables affecting DCP behavior

DCP has knowledge of a number of environment variables that can change its behavior; they are used mostly for testing.

| Variable | Description |
| --- | --------- |
| `DCP_BIN_PATH` and `DCP_EXTENSIONS_PATH` | These variables point to, respectively, the DCP root binary/configuration directory, and the DCP extensions directory. DCP root directory contains the main DCP CLI/API server binary and the default access configuration (kubeconfig) file. The extensions directory contains the DCP controller process binary and other extensions, if present. <br/> By default, DCP assumes the root directory to be `${HOME}/.dcp`. .NET Aspire tooling is using these environment variables to instruct DCP to use locations and binaries inside Aspire.Hosting.Orchestration workload instead. |
| `DEBUG_SESSION_PORT`, `DEBUG_SESSION_TOKEN`, and `DEBUG_SESSION_SERVER_CERTIFICATE` | These are variables that configure the endpoint for running Executables via a developer IDE/under debugger. For more information see [IDE execution specification](https://github.com/dotnet/aspire/blob/main/docs/specs/IDE-execution.md). |
| `DCP_SESSION_FOLDER` | This variable is used for isolating multiple DCP instances running concurrently on the same machine. If set (to a valid filesystem folder), DCP process(es) will create files related to their execution in this folder: the access configuration file (kubeconfig), captured Executable/Container logs, etc. |
| `DCP_LOG_SOCKET` | If set to a Unix domain socket, DCP will write its execution logs to that socket instead of writing them to standard error stream (`stderr`). This allows programs that launch DCP to capture its output even if DCP is running in `--detach` mode. <br/> The `--detach` mode causes DCP to fork itself and break the parent-child relationship (and lifetime dependency) from the process that launched it, but the side effect of doing so is that the parent process loses ability to monitor DCP standard output and standard error streamd. |
| `DCP_DIAGNOSTICS_LOG_LEVEL` | If set, enabled DCP diagnostic logging. <br/> Can be set to `error`, `info`, or `debug`; for troubleshooting `debug` is recommended, although it results in the most verbose output. |
| `DCP_DIAGNOSTICS_LOG_FOLDER` | If set to a valid filesystem folder, DCP will place the diagnostic logging files there. Otherwise (if enabled) they are written to the default temporary files folder. |
| `DCP_PERF_TRACE` | If set, instructs DCP to capture a performance trace during startup and/or shutdown. For more information see [performance investigations page](performance-investigations.md). |
| `DCP_PRESERVE_EXECUTABLE_LOGS` | If set (to "true", "yes", or "1"), the logs from Executables will not be deleted when DCP shuts down. This can be useful to capture results of test runs that use DCP as the workload orchestrator. |
| `DCP_RESOURCE_WATCH_TIMEOUT_SECONDS` | A timeout for resource watch requests, in seconds. Watch requests will time out shortly after the specified value, to avoid the "thundering herd" problem. Useful for testing watch retry logic. |
| `DCP_IDE_REQUEST_TIMEOUT_SECONDS` | A timeout for requests to IDE run session endpoint. Defaults to 30 seconds. |
| `DCP_INSTANCE_ID_PREFIX` | A prefix for the automatically generated, random DCP instance ID, which is attached to every communication with the IDE when running Executables via IDE. For more information see [IDE execution specification](https://github.com/dotnet/aspire/blob/main/docs/specs/IDE-execution.md). |
| `DCP_MOST_RECENT_PORT_LIFETIME`, `DCP_PORT_AVAILABILITY_CHECK_TIMEOUT`, `DCP_PORT_ALLOCATION_CHECK_TIMEOUT`, `DCP_PORT_ALLOCATIONS_PER_ROUND`, and `DCP_PORT_ALLOCATION_ROUND_DELAY` | These are parameters that govern the algorithm DCP is using to ensure that dynamically allocated ports are unique for the whole machine, even if multiple DCP instances are running. <br/> <br/> `DCP_MOST_RECENT_PORT_LIFETIME` is the time a recently allocated network port is considered unavailable. Used during automatic port allocation. Defaults to 2 minutes. Note that a particular port number will never be re-used if it is being *actively used* by a program running on the machine, even if that port has been auto-allocated earlier than "lifetime" ago. <br/> <br/> `DCP_PORT_AVAILABILITY_CHECK_TIMEOUT` determines how long DCP will try to access the most recently used ports file when checking for port availability. Defaults to 50 milliseconds. <br/> <br/> `DCP_PORT_ALLOCATION_CHECK_TIMEOUT` determines how long DCP will try to ensure that the allocated port is unique for the whole machine (per single port allocation). Defaults to 1 second. <br/> <br/> `DCP_PORT_ALLOCATIONS_PER_ROUND` determines how many port allocation attempts a DCP instance will make while holding a lock on the most recently used ports file. Defaults to 5. <br/> <br/> `DCP_PORT_ALLOCATION_ROUND_DELAY` tells DCP how long to wait (releasing the lock on the MRU ports file) before attempting to allocate a unique port again. Defaults to 100 ms. <br/> <br/> The values of all these environment variables should be the same as Go `time.Duration` literals (except from `DCP_PORT_ALLOCATIONS_PER_ROUND`, which is a positive integer). |
| `DCP_IP_VERSION_PREFERENCE` | Describes which version of the IP protocol will be preferred for communicating with DCP and for allocating service ports. Can be set to `IPv4`, `v4` or `4` (to prefer IP protocol version 4) or `IPv6`, `v6` or `6` (to prefer IP protocol version 6). <br/> <br/> The preference determines which type of available IP addresses will be preferred when resolving `localhost` host name. `localhost` is the default pseudo-address that both Services and DCP itself bind to. The value of `DCP_IP_VERSION_PREFERENCE` environment variable is not used if a Service requests specific address, or if a Service uses an address allocation mode that is different from `localhost`. |
