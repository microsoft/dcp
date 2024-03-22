# Performance investigations

## Taking a performance trace.

You can take a performance trace by setting `DCP_PERF_TRACE`. The format of the value is

`trace_type=duration,trace_type=duration,...`

Currently we only have two trace types: `startup` and `shutdown`. For example, setting the value to `startup=20s` will get you a 20s startup trace sent to `${TEMP}/dcp/logs` (or wherever `DCP_DIAGNOSTICS_LOG_FOLDER` environment variable points to).

The trace consists of a set of `pprof` files (one for `dcp` and one for `dcpctrl` respectively). You want to wait till the trace is collected before shutting down the app (e.g. Aspire dashboard). The trace is ready when all the `pprof` files have length greater than zero.

### Analyzing the trace

You can use the Go `pprof` tool to analyze the trace from all DCP executables, for example:

```shell
go tool pprof -http localhost:54321 dcp-startup-1696286089-34112.pprof dcpctrl-startup-1696286096-17028.pprof
```

The tool requires `graphviz` diagram generator, which you can install from `www.graphviz.org`, or via the package manager of your choice.

For startup investigation here are good root functions to analyze:

| Program | Root function |
| --- | --- |
| dcp CLI | `startApiSrv` or `runApp` |
| `dcpctrl` | `runControllers` |



