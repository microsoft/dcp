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

## Debugggin a dotnet CLI-based application start

(TODO)
