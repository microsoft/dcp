# Contributing to Developer Control Plane

## Ideas, feature requests, and bugs
We are open to all ideas and we want to get rid of bugs! Use the Issues section to either report a new issue, provide your ideas or contribute to existing threads.

## Learning about DCP
The **doc** contains several documents describing various aspects of DCP architecture and working. At its core, DCP is an Kubernetes-compatible API server and a set of custom Kubernetes types and controllers tailored for running multi-service applications on a developer machine. If you are new to authoring Kubernets types and controllers, a good starting point is [Authoring DCP Controllers page](/doc/authoring-dcp-controllers.md). 

## Legal

Before we can accept your pull request you will need to sign a **Contribution License Agreement**. All you need to do is to submit a pull request, then the PR will get appropriately labelled (e.g. `cla-required`, `cla-norequired`, `cla-signed`, `cla-already-signed`). If you already signed the agreement we will continue with reviewing the PR, otherwise system will tell you how you can sign the CLA. Once you sign the CLA all future PR's will be labeled as `cla-signed`.

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
Now you can run `go test` commands to run selected tests, including integration tests. For example, to run just Endpoint controller tests in verbose mode with race detection:

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
