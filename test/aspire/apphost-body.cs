// Aspire end-to-end regression AppHost for DCP.
//
// This file is the body of a file-based (single-file) Aspire AppHost that is
// run against a locally built DCP binary to catch integration regressions before
// a DCP build is inserted into Aspire. It intentionally does not contain a
// "#:sdk" directive: run-regression.sh scaffolds a fresh AppHost with the
// latest Aspire daily CLI, preserves the generated daily SDK directive, appends
// this body, and runs it.
//
// The AppHost starts the model, waits for every resource to become healthy, and
// exits (0 on success, non-zero on failure). The scenario includes both session
// and persistent containers. Resource names, container names, and container
// network aliases intentionally use mixed casing to exercise DCP's case handling
// (a past regression attached containers to the wrong network when network names
// were mixed case).

using Microsoft.Extensions.DependencyInjection;
using Aspire.Hosting.ApplicationModel;

var runId = Environment.GetEnvironmentVariable("DCP_ASPIRE_REGRESSION_RUN_ID");
if (string.IsNullOrWhiteSpace(runId))
{
    runId = "manual";
}

var builder = DistributedApplication.CreateBuilder(new DistributedApplicationOptions
{
    Args = args,
    DisableDashboard = true,
});

// Executable resource: exercises DCP process orchestration.
var worker = builder.AddExecutable("worker", "sleep", ".", "3600");

// Session container resource: exercises DCP container creation and cleanup.
var cache = builder.AddContainer("cache", "mcr.microsoft.com/cbl-mariner/busybox", "2.0")
    .WithEntrypoint("sleep")
    .WithArgs("3600")
    .WithEnvironment("DCP_ASPIRE_REGRESSION_RUN_ID", runId)
    .WithContainerNetworkAlias("Cache-Alias");

// Persistent containers: exercises Aspire's persistent container path and the
// DCP persistent network resources it creates for persistent containers.
var persistentCache = builder.AddContainer("persistent-cache", "mcr.microsoft.com/cbl-mariner/busybox", "2.0")
    .WithEntrypoint("sleep")
    .WithArgs("3600")
    .WithEnvironment("DCP_ASPIRE_REGRESSION_RUN_ID", runId)
    .WithLifetime(ContainerLifetime.Persistent)
    .WithContainerName($"Persistent-Cache-MixedCase-{runId}")
    .WithContainerNetworkAlias("Persistent-Cache-Alias");

var persistentWorker = builder.AddContainer("persistent-worker", "mcr.microsoft.com/cbl-mariner/busybox", "2.0")
    .WithEntrypoint("sleep")
    .WithArgs("3600")
    .WithEnvironment("DCP_ASPIRE_REGRESSION_RUN_ID", runId)
    .WithLifetime(ContainerLifetime.Persistent)
    .WithContainerNetworkAlias("Persistent-Worker-Alias")
    .WaitFor(persistentCache);

// Session container depending on both session and persistent containers:
// exercises container network attachment and dependency ordering.
var web = builder.AddContainer("web", "mcr.microsoft.com/cbl-mariner/busybox", "2.0")
    .WithEntrypoint("sleep")
    .WithArgs("3600")
    .WithEnvironment("DCP_ASPIRE_REGRESSION_RUN_ID", runId)
    .WithContainerNetworkAlias("Web-Alias")
    .WaitFor(cache)
    .WaitFor(persistentWorker);

await using var app = builder.Build();
using var cts = new CancellationTokenSource(TimeSpan.FromMinutes(5));

try
{
    await app.StartAsync(cts.Token);
    Console.WriteLine("DCP-REGRESSION-APPHOST-STARTED");

    var notifications = app.Services.GetRequiredService<ResourceNotificationService>();
    foreach (var resourceName in new[] { "worker", "cache", "persistent-cache", "persistent-worker", "web" })
    {
        await notifications.WaitForResourceHealthyAsync(resourceName, cts.Token);
        Console.WriteLine($"DCP-REGRESSION-RESOURCE-HEALTHY: {resourceName}");
    }

    Console.WriteLine("DCP-REGRESSION-OK: all resources healthy");
}
finally
{
    await app.StopAsync(cts.Token);
}
