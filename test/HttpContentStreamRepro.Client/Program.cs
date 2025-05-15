using HttpContentStreamRepro.Client;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;

var builder = Host.CreateApplicationBuilder(args);

builder.Services
    .Configure<ReaderOptions>(options =>
    {
        options.BatchSize = 100;
        options.ChunkSize = 4_000_000;
        options.Delay = TimeSpan.FromMilliseconds(15);
        options.FillBuffer = true;
        options.StreamSource = StreamSource.Http;
    })
    .AddTransient<Reader>()
    .AddHttpClient<Reader>(client => client.BaseAddress = new(Environment.GetEnvironmentVariable("SERVER_URL") ?? "http://localhost:5000"));

var app = builder.Build();

await using var reader = app.Services.GetRequiredService<Reader>();
await using var stream = await reader.GetStreamAsync();

await reader.ReadStreamAsync(stream);