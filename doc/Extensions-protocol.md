# DCP extensions protocol

A DCP extension is a program that interacts with DCP API server and updates the running application workload model. The following types of extensions are supported today:

- **Controllers** react to changes to the workload model,  and strive to make the running application conform to the workload model (reconciliation). They also augment the workload model with detailed information about the running application.

- **Workload renderers** examine the source code of an application and generate a workload model for the application based on the source code.

- **API server** is the Kubernetes-compatible API server that holds the workload model(s) and facilitates information exchange between DCP CLI, controllers, and workload renderers. There must be exactly one API server available for the CLI.

> The same program can function as a controller and as a workload renderer, but it will be invoked differently depending on desired role.

> Replacing the API server is an advanced scenario that is not currently supported.

The extension protocol is a set of conventions for invoking extension programs. The protocol includes commands, command parameters, expected response schemas, ways to request extenion shutdown, and conventions for exit codes.


## Installation
Extension programs should be placed in the `ext` sub-directory of the DCP installation directory. Any executable placed there will be interrogated about its capabilities ([see `get-capabilities` command](#get-capabilities)).


## Common conventions

The following conventions apply to all commands.

### Command output
Command output that is meant to be consumed by DCP CLI (machine-readable data) should be written to standard output (`stdout`) in JSON format. The output should be terminated by new line character. Standard output **should not be used** for human-readable data or error messages. Those should be written to standard error (`stderr`) only.

### Exit codes
Upon successful execution the command should return zero as the exit code. Non-zero code indicates failure.

### Common parameters

| Parameter | Description | Mandatory |
| --- | ------ | --- |
| `--v <level>` | Verbosity level. Zero means normal verbosity (only errors should be reported--to standard output). Any non-zero value means higher verbosity, the higer the value, the more "chatty" the program should be. | No. If absent, assume normal ("zero") verbosity level. |


## Commands

### `get-capabilities` (all extensions)

This it the first command that DCP will invoke on the extension program. It allows the extension to declare whether it is a controller, workload renderer, or both. The command takes no parameters.

The extension should reply with a JSON document that declares its capabilities, for example:

```shell
> my-controller get-capabilties
{
    "name": "My Controller",
    "id": "my-controller",
    "capabilities": [ "controller" ]
}
```

The value of the `name` property will be used by DCP CLI in any prompts or error messages when referring to the extension. The extension identifier (`id` property) is used when referring to the extension via command-line arguments (e.g. `--app-type` argument of the DCP `up` command). The output document should conform to [capabilities document schema](https://github.com/usvc-dev/apiserver/schemas/v1.0/capabilities.json).

If there is an error, a non-zero exit code should be used, and the error message should be written to `stderr`. If an executable program placed in the extensions directory does not declare its capabilites as expected, DCP CLI will warn about it, but continue execution using other extensions.

### `run-controllers` (controller extensions)

The `run-controllers` command is a request to run any contollers the extension may host. 

| Parameter | Description | Mandatory |
| --- | ------ | --- |
| `--kubeconfig <path>` | The path to `kubeconfig` file that should be used to connect to the API server. Extensions can assume that the current context specified in the file is the correct context to use (no need to prompt for context even if multiple contexts are defined). | Yes |
 
On this command the extension should start its controller(s) and keep them running until the extension program receives `SIGINT` or `SIGTERM` signal. Upon receiving one of these signals, controllers should be shut down gracefully, and the extension program should exit with code zero. 

No machine-readable output is expected from the command. If there is an error preventing controller execution, the extension program should write the error message to standard error and exit with non-zero exit code. Depending on verbosity level (see [common parameters](#common-parameters)), the extension might write diagnostic/informational messages to standard error.

### `render-workload` (workload renderer extensions)

The `render-workload` command is a request to analyze application code and produce the workload definition that will allow the user to test and debug the application.

| Parameter | Description | Mandatory |
| --- | ------ | --- |
| `--root-dir <path>` or `-r <path>` | The path to application root directory. The rendered should start its analysis from this directory. | Yes |
| `--kubeconfig <path>` | The path to `kubeconfig` file that should be used to connect to the API server. Extensions can assume that the current context specified in the file is the correct context to use (no need to prompt for context even if multiple contexts are defined). | Yes |

After the application is analyzed, the extension should connect to the API server and create necessary DCP objects (Executables, Containers etc) to instantiate the application. To the greatest extent possible, the rendering should be an atomic operation, that is, the renderer should create all application workload objects, or report an error without creating anything.

No machine-readable output is expected from the command. If an error occurs, the extension should write the error message to standard error and exit with non-zero exit code. Depending on verbosity level (see [common parameters](#common-parameters)), the extension might write diagnostic/informational messages to standard error.

### `can-render` (workload renderer extensions)

The `can-render` command is a request to determine whether the extension can render a workload for a application. Renderers specialize in different kinds of applications; for a given application, zero, one, or more than one renderer may be capable of rendering a workload for it.

| Parameter | Description | Mandatory |
| --- | ------ | --- |
| `--root-dir <path>` or `-r <path>` | The path to application root directory. The rendered should start its analysis from this directory. | Yes |

The renderer communicates the result of the analysis via exit code a JSON document written to standard output:

```shell
> azure-dev-renderer can-render --appRootDir /home/john/repos/my-web-site
{
    "result": "no",
    "reason": "This renderer only works with applications that have been configured to use Azure Developer CLI. For more information see https://aka.ms/azure-dev"
}
```

The output document should conform to [`can-render` command response schema](https://github.com/usvc-dev/apiserver/schemas/v1.0/can-render.json). The `reason` property is optional if the result is positive, but it is mandatory if the result is negative (workload rendering is impossible).

If an error occurs during analysis, the extension should write the error message to standard error and exit with non-zero exit code. Depending on verbosity level (see [common parameters](#common-parameters)), the extension might write diagnostic/informational messages to standard error.
