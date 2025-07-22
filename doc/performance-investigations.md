# Performance investigations

DCP can produce performance traces in `pprof` format that is natively supported by Go tooling. The following types of traces are supported:

| Trace type | Description |
| --- | --- |
| `startup` | Collected when DCP and DCP controller processes start. Measures wall clock time (both CPU and IO time). |
| `shutdown` | Collected during shutdown resource cleanup. Measures wall clock time (both CPU and IO time). |
| `snapshot` | Collected at any point during DCP execution, on request. Measures wall clock time (both CPU and IO time). |
| `startup-cpu` | Collected when DCP and DCP controller processes start. Measures CPU time only (IO time is ignored). |
| `shutdown-cpu` | Collected during shutdown resource cleanup. Measures CPU time only (IO time is ignored). |
| `snapshot-cpu` | Collected at any point during DCP execution, on request. Measures CPU time only (IO time is ignored). |

The trace consists of a set of `pprof` files (one for `dcp` and one for `dcpctrl` respectively). You want to wait till the trace is collected before shutting down the app that is using DCP. The trace is ready when all the `pprof` files have length greater than zero. Trace files are put in `${TEMP}/dcp/logs`, but this can be overridden via `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable.

## Taking a startup or shutdown performance trace

You can take a performance startup or shutdown trace by setting `DCP_PERF_TRACE` before launching DCP. The format of the value is

`trace_type=duration,trace_type=duration,...`

For example, setting the value to `startup=20s` will get you a 20s trace collected from the moment DCP and DCP controller processes start. 

## Taking a "snapshot" performance trace

A performance trace ("snapshot" type) can be collected at any time using DCP administrative endpoint. The endpoint is aggregated with the API server, but it does not constitute a standard part of Kubernetes API, so you will need to use `kubectl` tool to make a secure proxy connection to the API server and interrogate the endpoint via raw HTTP using `curl` or similar tool. The `kk` script that wraps `kubectl` and makes it easy to discover running DCP instance can be found in `scripts` directory in this repo. For example:

```shell
# Start the API server endpoint proxy.
> kk proxy --port=0
Starting to serve on 127.0.0.1:51437

# (then, from a separate shell; change duration & type values as necessary)
curl -X PUT "http://127.0.0.1:51437/admin/perftrace?duration=10s&type=snapshot-cpu"
```

## Analyzing the trace

You can use the Go `pprof` tool to analyze the trace from all DCP executables, for example:

```shell
# (replace names of pprof files as necessary)
go tool pprof -http localhost:37114 dcp-startup-1696286089-34112.pprof dcpctrl-startup-1696286096-17028.pprof
```

The tool requires `graphviz` diagram generator, which you can install from `www.graphviz.org`, or via the package manager of your choice.

For startup investigation here are good root functions to analyze:

| Program | Root function |
| --- | --- |
| `dcp` (DCP API server) | `startApiSrv` |
| `dcpctrl` (the controllers process) | `runControllers` |
| `dcptun` (DCP reverse tunnel) | `runClientProxy` or `runServerProxy` |
