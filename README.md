# DCP monorepo <!-- omit from toc -->

- [Development environment setup](#development-environment-setup)
  - [Go module system setup](#go-module-system-setup)
  - [GitHub authentication setup](#github-authentication-setup)
- [Making DCP available from $PATH](#making-dcp-available-from-path)
  - [macOS, Linux, WSL](#macos-linux-wsl)
  - [Windows](#windows)
- [Running tests](#running-tests)
  - [Run all tests](#run-all-tests)
  - [Running subsets of tests from command line](#running-subsets-of-tests-from-command-line)
  - [Debugging tests](#debugging-tests)
  - [Running Aspire tests](#running-aspire-tests)
- [Troubleshooting and debugging tips](#troubleshooting-and-debugging-tips)
  - [`make lint` times out (or ends with an error that says "Killed")](#make-lint-times-out-or-ends-with-an-error-that-says-killed)
  - [Make it easier to use `kubectl` with DCP](#make-it-easier-to-use-kubectl-with-dcp)
  - [After `make generate-openapi` the generated file is empty (almost all contents has been removed).](#after-make-generate-openapi-the-generated-file-is-empty-almost-all-contents-has-been-removed)
  - [I need to test a local build of `dcp` with Aspire tooling](#i-need-to-test-a-local-build-of-dcp-with-aspire-tooling)
  - [Need to get detailed logs from DCP run](#need-to-get-detailed-logs-from-dcp-run)
  - [I need to debug DCP](#i-need-to-debug-dcp)
  - [I need to debug DCP controllers in the context of an Aspire (Visual Studio-based) application run](#i-need-to-debug-dcp-controllers-in-the-context-of-an-aspire-visual-studio-based-application-run)
  - [Taking performance traces](#taking-performance-traces)
  - [Environment variables affecting DCP behavior](#environment-variables-affecting-dcp-behavior)

This repository contains the core components of the Developer Control Plane tool:
-  `dcp` CLI that users invoke to run applications and for other, related tasks. If invoked with `start-apiserver` it will act as the API server that holds the workload model used by controllers and API providers to create a workload definition, run it, and expose information about it. The API server is Kubernetes-compatible. It is implemented using [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.
-  `dcpctrl` is the core DCP controllers that implement the standard behavior for DCP workload models.
- `dcpproc` is a tool for ensuring that a given process or container is stopped when monitored "parent" process exits. It provides additional assurance that no workload resoures are abandoned and left behind after an application run.
- `dcptun` is a program that implements a reverse network tunnel. It is driven by Kubernetes API and tightly integrated with the rest of DCP.


## Development environment setup
You will need:
- [Go](https://go.dev/dl/)
- `make` tool

Supported operating systems for development are Linux, MacOS, and Windows. On Windows, in addition to the `make` tool, you will need the following command-line tools to be installed and on `PATH`:
- awk
- curl
- golangci-lint

On Windows you can install all the tools with [Chocolatey](https://chocolatey.org/) (from an elevated prompt):

```powershell
choco install make /y
choco install awk /y
choco install curl /y
choco install golangci-lint /y
```

...or [scoop](scoop.sh) (this doesn't need elevation):

```powershell
scoop install make
scoop install gawk
scoop install curl
scoop install golangci-lint
```

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

## Running Aspire tests
You can run the Aspire tests against a local DCP instance using the `DcpPublisher__CliPath` environment variable. Set it with the absolute path to a local DCP executable, e.g. `DcpPublisher__CliPath=<dcprepo>/bin/dcp` and when you run Aspire tests, they'll use your local build instead of the current version inserted into the Aspire repo.

## Troubleshooting and debugging tips

### `make lint` times out (or ends with an error that says "Killed")
We have seen the linter occasionally go into a persistent, bad state. Do `make clean`, then retry `make lint` again.

### Make it easier to use `kubectl` with DCP
Aspire tooling is invoking DCP in "session" mode, which creates a separate `kubeconfig` file for every application run. If you want to interrogate the API server with `kubectl`, you must point it to the right `kubeconfig` file, there is no default. To help with that, we have created a script called `kk` (there is a PowerShell version for Windows development, and a Bash version for Mac/Linux). You will find it in the `scripts` folder in the repository.

`kk` script will search for DCP process(es) running on the system and extract the `kubeconfig` path from its launch parameters. It will also extract the security token if DCP is using one externally supplied. Then the script will launch `kubectl` with the parameters you provided, but also pointing it to the DCP (session) `kubeconfig` file and supplying the security token, if any. For example, to display a list of running Executable objects do `kk get exe`.

> Hint: to list all available types of DCP objects, do `kk api-resources`.


### After `make generate-openapi` the generated file is empty (almost all contents has been removed).
Looks like the OpenAPI code generator failed. Run `make generate-openapi-debug` to enable debug output and check if it contains any clues.

We have seen an issue where the generator would latch to a specific version of `go` compiler and and fail when the compiler is updated. Deleting `.toolbin/openapi-gen` binary usually helps in this case.


### I need to test a local build of `dcp` with Aspire tooling

Add the following snippet to your AppHost project:

```xml
    <PropertyGroup>
      <DcpDir>[folder where dcp.exe lives, e.g. <repo root>\bin\]</DcpDir>
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

For the change to take effect, you might need to rebuild the AppHost project in Visual Studio.

### Need to get detailed logs from DCP run

Set the `DCP_DIAGNOSTICS_LOG_LEVEL` environment variable to `debug`. The logs will be put to `${TEMP}/dcp/logs`. You can change the destination for the log files by setting `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable.

### I need to debug DCP

There are several VS Code debug configurations available for this repository. They are very straightforward; take a look at `launch.json` and choose one that fits your needs best, or create a custom one for your scenario.

If you need to learn morea about Go debugging in VS Code, [VS Code Go debugging wiki](https://github.com/golang/vscode-go/wiki/debugging) and [documentation for the underlying delve Go debugger](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) might be helpful.

> Note: you want to use `make compile-debug` for building DCP for debugging. By default DCP is built with optimizations on, which can result in strange behavior during debugging (somewhat unpredictable order of statements, local data "disappearing" in the middle of a function etc.)

> Tip: if you need to debug the API server while debugging a test (testing controller-API server interaction), opening a new VS Code window and trying to open the DCP repository does not quite work, because VS Code aggressively reuses existing windows and will switch you back to the window that is running the test.
>
> To work around that use "Duplicate as workspace in new window" command from VS Code command palette.

### I need to debug DCP controllers in the context of an Aspire (Visual Studio-based) application run

The following procedure can be used to debug DCP controllers when an application is run from Visual Studio or Visual Studio Code (with C# DevKit installed):

1. Open the solution with your application in Visual Studio or Visual Studio Code.
1. See the section above for "I need to test a local build of `dcp` with Aspire tooling".
1. Set a breakpoint in the `DcpExecutor.RunApplicationAsync` method. The class is in the `Aspire.Hosting.Dcp` namespace. You can use a "Function Breakpoint" that refers to the fully-qualified name of the method; you do not need to have the `Aspire.Hosting` project in your solution. **Do not forget to disable the "Just My Code" option in debugger settings**.
1. Open `dcp` repository in a separate instance of Visual Studio Code.
1. Run the application. When the breakpoint is hit, the DCP API server and controller host should already be started, but no workload objects have been created yet.
1. Switch to Visual Studio Code instance that has DCP sources open, select `attach to controllers process` debug configuration and start debugging. When prompted, select the `dcpctrl` process (there should be just one). Set breakpoints in controller code as necessary.
1. Switch back to your application IDE and continue (F5). The application workload definition will be created by the `ApplicationExecutor` and sent to DCP for execution.

The same steps can also be used to:
- Debug `ApplicationExecutor` (in Visual Studio) if you suspect that the workload that DCP receives is set up incorrectly.
- Debug code that is hosted by the DCP API server (e.g. log storage/streaming), except that you want to attach to the API server process instead, using `attach to API server process` debug configuration.

> Tip: if the API server request is failing before hitting DCP code, a good place to put a breakpoint is one of the endpoint handlers in `${GOPATH}/pkg/mod/k8s.io/apiserver@{version}/pkg/endpoints/handlers`. You can figure out which `apiserver` package version DCP is using from the `go.mod` file, and your `${GOPATH}` value can be learned by running `go env` command.

### Advanced tests

Not all tests are run by default. A small subset of tests aren't suitable for running in a CI/CD pipeline due to their nature (they require a specific environment or are too slow). In addition, some networking tests will trigger firewall prompts when run on Windows. These tests are skipped by the `test` and `test-ci` `make` targets by default, but you can enable them (for `make`-based and `go test`-based runs) via environment variables:

| Variable | Tests enabled |
| --- | --------- |
| `DCP_TEST_ENABLE_ADVANCED_NETWORKING` | Set to true to enable advanced networking tests such as those that require access to all network interfaces and try to open ports that are accessible to requests originating from outside of the machine, or tests that evaluate network performance against a baseline (these are unreliable on CI machines). |
| `DCP_TEST_ENABLE_ADVANCED_CERTIFICATES`| Set to true to enable advanced certificate file tests such as those that require openssl installed to verify behavior. |
| `DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR` | Set to true to enable tests that require real container orchestrator (Docker or Podman). |

> You may need to install [Azure Artifacts Credentials Provider](https://github.com/microsoft/artifacts-credprovider#azure-artifacts-credential-provider) to be able to build some of the test artifacts for tests in this set.

### Taking performance traces

See [performance investigations page](doc/performance-investigations.md).

### Environment variables affecting DCP behavior

DCP has knowledge of a number of environment variables that can change its behavior; they are used mostly for testing.

| Variable | Description |
| --- | --------- |
| `DCP_BIN_PATH` and `DCP_EXTENSIONS_PATH` | These variables point to, respectively, the DCP root binary/configuration directory, and the DCP extensions directory. DCP root directory contains the main DCP CLI/API server binary and the default access configuration (kubeconfig) file. The extensions directory contains the DCP controller process binary and other extensions, if present. <br/> By default, DCP assumes the root directory to be `${HOME}/.dcp`. .NET Aspire tooling is using these environment variables to instruct DCP to use locations and binaries inside Aspire.Hosting.Orchestration workload instead. |
| `DEBUG_SESSION_PORT`, `DEBUG_SESSION_TOKEN`, and `DEBUG_SESSION_SERVER_CERTIFICATE` | These are variables that configure the endpoint for running Executables via a developer IDE/under debugger. For more information see [IDE execution specification](https://github.com/dotnet/aspire/blob/main/docs/specs/IDE-execution.md). |
| `DCP_SESSION_FOLDER` | This variable is used for isolating multiple DCP instances running concurrently on the same machine. If set (to a valid filesystem folder), DCP process(es) will create files related to their execution in this folder: the access configuration file (kubeconfig), captured Executable/Container logs, etc. |
| `DCP_LOG_SOCKET` | If set to a Unix domain socket, DCP will write its execution logs to that socket instead of writing them to standard error stream (`stderr`). This allows programs that launch DCP to capture its output even if DCP is running in `--detach` mode. <br/> The `--detach` mode causes DCP to fork itself and break the parent-child relationship (and lifetime dependency) from the process that launched it, but the side effect of doing so is that the parent process loses ability to monitor DCP standard output and standard error streamd. |
| `DCP_LOG_SESSION_ID` | If set, DCP will prepend this value to all diagnostics log names. If unset, a session ID will be calculated. The value is propagated to all child DCP processes. |
| `DCP_DIAGNOSTICS_LOG_LEVEL` | If set, enabled DCP diagnostic logging. <br/> Can be set to `error`, `info`, or `debug`; for troubleshooting `debug` is recommended, although it results in the most verbose output. |
| `DCP_DIAGNOSTICS_LOG_FOLDER` | If set to a valid filesystem folder, DCP will place the diagnostic logging files there. Otherwise (if enabled) they are written to the default temporary files folder. |
| `DCP_LOG_FILE_NAME_SUFFIX` | Suffix to append to the log file name (defaults to process ID if not set). |
| `DCP_LOGGING_CONTEXT` | If set, the value of this variable will be written to the log file as one of the first log messages (as verbose, "info" type of message). |
| `DCP_PRESERVE_EXECUTABLE_LOGS` | If set (to "true", "yes", or "1"), the logs from Executables will not be deleted when DCP shuts down. This can be useful to capture results of test runs that use DCP as the workload orchestrator. |
| `DCP_RESOURCE_WATCH_TIMEOUT_SECONDS` | A timeout for resource watch requests, in seconds. Watch requests will time out shortly after the specified value, to avoid the "thundering herd" problem. Useful for testing watch retry logic. |
| `DCP_IDE_REQUEST_TIMEOUT_SECONDS`, `DCP_IDE_NOTIFICATION_TIMEOUT_SECONDS`, and  `DCP_IDE_NOTIFICATION_KEEPALIVE_SECONDS`  | Timeouts for, respectively: requests to IDE run session endpoint (defaults to 120 seconds), request to IDE notification WebSocket endpoint (defaults to 20 seconds), and the time to interpret the lack of response to WebSocket ping message on the IDE notification connection is interpreted as connection failue (defaults to 5 seconds). <br/> The IDE notification keepalive value, if present, must be smaller than the IDE notification request timeout value. <br/> A value of zero for IDE notification keepalive turns off the keep-alives (ping-pong messages) for the IDE notification connection. |
| `DCP_INSTANCE_ID_PREFIX` | A prefix for the automatically generated, random DCP instance ID, which is attached to every communication with the IDE when running Executables via IDE. For more information see [IDE execution specification](https://github.com/dotnet/aspire/blob/main/docs/specs/IDE-execution.md). |
| `DCP_MOST_RECENT_PORT_LIFETIME`, `DCP_PORT_AVAILABILITY_CHECK_TIMEOUT`, `DCP_PORT_ALLOCATION_CHECK_TIMEOUT`, `DCP_PORT_ALLOCATIONS_PER_ROUND`, and `DCP_PORT_ALLOCATION_ROUND_DELAY` | These are parameters that govern the algorithm DCP is using to ensure that dynamically allocated ports are unique for the whole machine, even if multiple DCP instances are running. <br/> <br/> `DCP_MOST_RECENT_PORT_LIFETIME` is the time a recently allocated network port is considered unavailable. Used during automatic port allocation. Defaults to 2 minutes. Note that a particular port number will never be re-used if it is being *actively used* by a program running on the machine, even if that port has been auto-allocated earlier than "lifetime" ago. <br/> <br/> `DCP_PORT_AVAILABILITY_CHECK_TIMEOUT` determines how long DCP will try to access the most recently used ports file when checking for port availability. Defaults to 50 milliseconds. <br/> <br/> `DCP_PORT_ALLOCATION_CHECK_TIMEOUT` determines how long DCP will try to ensure that the allocated port is unique for the whole machine (per single port allocation). Defaults to 1 second. <br/> <br/> `DCP_PORT_ALLOCATIONS_PER_ROUND` determines how many port allocation attempts a DCP instance will make while holding a lock on the most recently used ports file. Defaults to 5. <br/> <br/> `DCP_PORT_ALLOCATION_ROUND_DELAY` tells DCP how long to wait (releasing the lock on the MRU ports file) before attempting to allocate a unique port again. Defaults to 100 ms. <br/> <br/> The values of all these environment variables should be the same as Go `time.Duration` literals (except from `DCP_PORT_ALLOCATIONS_PER_ROUND`, which is a positive integer). |
| `DCP_IP_VERSION_PREFERENCE` | Describes which version of the IP protocol will be preferred for communicating with DCP and for allocating service ports. Can be set to `IPv4`, `v4` or `4` (to prefer IP protocol version 4) or `IPv6`, `v6` or `6` (to prefer IP protocol version 6). <br/> <br/> The preference determines which type of available IP addresses will be preferred when resolving `localhost` host name. `localhost` is the default pseudo-address that both Services and DCP itself bind to. The value of `DCP_IP_VERSION_PREFERENCE` environment variable is not used if a Service requests specific address, or if a Service uses an address allocation mode that is different from `localhost`. |
| `DCP_SHUTDOWN_TIMEOUT_SECONDS` | Overrides the default DCP shutdown timeout (120 seconds). DCP will use this timeout to wait for all resources to be cleaned up before shutting down. |
| `DCP_SECURE_TOKEN` | Provides a predetermined bearer token for DCP to use rather than generating a randm one. If this is set, DCP will write a placeholder value to the kubeconfig file. Any client trying to connect will have to know this predetermined token and apply it to their config themselves. |
| `DCP_PERF_TRACE` | If set, instructs DCP to capture a performance trace during startup and/or shutdown. For more information see [performance investigations page](performance-investigations.md). |
| `DCP_DISABLE_PROCESS_CLEANUP_JOB` | On Windows, DCP will use a Win32 Job object to ensure that processes it launches are terminated when it shuts down. This alters standard process startup sequence and might not work on tightly locked-down machines, or might be flagged as "suspicious behavior" by antivirus software. If DCP cannot start processes and complains about "process cleanup job" errors, setting `DCP_DISABLE_PROCESS_CLEANUP_JOB` to `1` or `true` should help. |
