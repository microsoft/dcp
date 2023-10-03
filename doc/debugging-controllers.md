# Debugging DCP controllers

## Debugging a Visual Studio-based application start

The following procedure can be used to debug DCP controllers when an application is run from Visual Studio:

1. Open the solution with your application in Visual Studio and make sure the solution also includes `Microsoft.Extensions.Astra.Hosting.Dcp` project. <br/><br/>
1. Set a breakpoint in `ApplicationExecutor.RunApplicationAsync` method. <br/><br/>
1. Open `usvc-apiserver` repository in Visual Studio Code. <br/><br/>
1. Run the application. When the breakpoint is hit, the DCP API server and controller host should already be started, but no workload objects have been created yet. <br/><br/>
1. Switch to Visual Studio Code, select `attach to controller process` debug configuration and start debugging. When prompted, select the `dcpctrl` process (there should be just one). Set breakpoints in controller code as necessary. <br/><br/>
1. Switch back to Visual Studio and continue (F5). The workload definition will be created by the `ApplicationExecutor` and sent to DCP for execution.

The same steps can also be used to debug `ApplicationExecutor` (in Visual Studio) if you suspect that the workload that DCP receives is set up incorrectly.

## Debugging a dotnet CLI-based application start

(TODO)

## Get detailed logs

Set the `DCP_DIAGNOSTICS_LOG_LEVEL` environment variable to `debug`. The logs will be put to `${TEMP}/dcp/logs`, although you can change the destination by setting `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable.\
## Taking a performance trace.

You can take a performance trace by setting `DCP_PERF_TRACE`. The format of the value is 

`trace_type=duration,trace_type=duration,...`

Currently we only have two trace types: `startup` and `shutdown`. For example, setting the value to `startup=20s` will get you a 20s startup trace sent to `${TEMP}/dcp/logs` (or wherever `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable points to).

The trace consists of a set of `pprof` files (one for `dcp`, `dcpctrl`, and `dcpd`, respectively). You want to wait till the trace is collected before shutting down the app (e.g. Aspire dashboard). The trace is ready when all the `pprof` files have length greater than zero.

### Analyzing the trace

You can use the Go `pprof` tool to analyze the trace from all DCP executables, for example:

```shell
go tool pprof -http localhost:54321 dcp-startup-1696286089-34112.pprof dcpctrl-startup-1696286096-17028.pprof dcpd-startup-1696286096-17376.pprof
```

The tool requires `graphviz` diagram generator, which you can install from `www.graphviz.org`, or via the package manager of your choice.

For startup investigation here are good root functions to analyze:

| Program | Root function |
| --- | --- |
| dcp CLI | `startApiSrv` or `runApp` |
| `dcpd` | `runApiServer` |
| `dcpctrl` | `runControllers` |



