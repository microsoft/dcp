using System.Text;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace HttpContentStreamRepro.Client;

/// <summary>
/// Reads and processes the response stream using a buffer
/// </summary>
/// <remarks>
/// <paramref name="fillBuffer"/>: <see langword="true"/> (<see cref="ReadUntilFullAsync"/>) uses an algorithm similar to
/// <see href="https://github.com/googleapis/google-api-dotnet-client/blob/main/Src/Support/Google.Apis/Download/MediaDownloader.cs"/>
/// used by <see href="https://www.nuget.org/packages/Google.Cloud.Storage.V1"/>
/// </remarks>
public sealed class Reader(HttpClient httpClient, ILogger<Reader> logger, IOptions<ReaderOptions> options) : IAsyncDisposable
{
    private Task? _localStreamTask;

    public async ValueTask<Stream> GetStreamAsync(CancellationToken cancellationToken = default)
    {
        switch (options.Value.StreamSource)
        {
            // get the stream from HttpClient.Content, which seems related to the issue
            case StreamSource.Http:
                var response = await httpClient
                    .GetAsync("/values.csv", HttpCompletionOption.ResponseHeadersRead, cancellationToken)
                    .ConfigureAwait(false);

                return await response.Content.ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);

            // get a local stream which doesn't appear to trigger the issue (for comparison)
            case StreamSource.Local:
                var stream = new ReadWriteStream();
                var writeOptions = WriteOptions.Local(logger);
                _localStreamTask =
                    Task.Run(() => Line.WriteLinesAsync(stream, writeOptions, cancellationToken), cancellationToken);

                return stream;

            default:
                throw new InvalidOperationException();
        }
    }

    // in a loop, read the stream into a buffer and process each line
    public async Task ReadStreamAsync(Stream stream, CancellationToken cancellationToken = default)
    {
        var buffer = new byte[options.Value.ChunkSize];
        var bytesConsumed = 0L;
        var offset = 0;
        var row = 0L;

        while (true)
        {
            var length = options.Value.FillBuffer
                ? await ReadUntilFullAsync(stream, buffer.AsMemory(offset), cancellationToken).ConfigureAwait(false)
                : await ReadOnceAsync(stream, buffer.AsMemory(offset), cancellationToken).ConfigureAwait(false);

            if (offset + length == 0)
                break;

            var memory = buffer.AsMemory(0, offset + length);
            while (TryReadLine(ref memory, out var line))
            {
                bytesConsumed += line.Length + 1;
                await ProcessLineAsync(line, row, bytesConsumed).ConfigureAwait(false);
                row++;

                if (row % 10_000 == 0)
                    logger.LogInformation("Reader: Processed {Count:N0} lines, ~{BytesConsumed:N0} bytes", row, bytesConsumed);
            }

            offset = memory.Length;
            if (memory.Length > 0)
                memory.CopyTo(buffer);
        }
    }

    // only call stream.ReadAsync once per iteration
    // this implementation doesn't appear to trigger the issue
    private static ValueTask<int> ReadOnceAsync(
        Stream stream, Memory<byte> buffer, CancellationToken cancellationToken)
        => stream.ReadAsync(buffer, cancellationToken);

    // call stream.ReadAsync until the buffer is full or stream.ReadAsync returns 0
    // this implementation seems to trigger the issue
    private static async ValueTask<int> ReadUntilFullAsync(
        Stream stream, Memory<byte> buffer, CancellationToken cancellationToken)
    {
        var count = 0;
        while (count < buffer.Length)
        {
            var read = await stream.ReadAsync(buffer[count..], cancellationToken).ConfigureAwait(false);
            if (read == 0)
                break;

            count += read;
        }

        return count;
    }

    private async Task ProcessLineAsync(ReadOnlyMemory<byte> line, long row, long bytes)
    {
        ProcessLineCore(line.Span, row, bytes);

        if (row > 0 && row % options.Value.BatchSize == 0)
            await ProcessBatchAsync().ConfigureAwait(false);
    }

    // do occasional I/O which seems to help trigger the issue
    private Task ProcessBatchAsync()
        => Task.WhenAll(Enumerable.Range(0, options.Value.BatchSize).Select(_ => Task.Delay(options.Value.Delay)));

    // compare the line to the expected value
    private void ProcessLineCore(ReadOnlySpan<byte> line, long row, long bytes)
    {
        if (line.IsEmpty)
            return;

        Span<byte> expected = stackalloc byte[Line.MaxLineLength];
        var expectedLength = Line.FormatLine(expected, row, eol: false);
        expected = expected[..expectedLength];

        if (!line.SequenceEqual(expected))
        {
            var lineText = Encoding.UTF8.GetString(line);
            var expectedText = Encoding.UTF8.GetString(expected);
            logger.LogError(
                "Reader: Line was corrupted at row {Row:N0}, ~{BytesConsumed:N0} bytes:\nActual:    '{Actual}'\nExpected:  '{Expected}'",
                row, bytes, lineText, expectedText);
            throw new InvalidOperationException($"Line was corrupted at row {row:N0}, ~{bytes:N0} bytes: '{lineText}'");
        }
    }

    private static bool TryReadLine(ref Memory<byte> buffer, out ReadOnlyMemory<byte> line)
    {
        var span = buffer.Span;

        // Look for a EOL in the buffer.
        var index = span.IndexOf((byte)'\n');
        if (index == -1)
        {
            line = null;
            return false;
        }

        // Skip the line + the \n.
        line = buffer[..index];
        buffer = buffer[(index + 1)..];
        return true;
    }

    public async ValueTask DisposeAsync()
    {
        if (_localStreamTask is not null)
            await _localStreamTask.ConfigureAwait(false);
    }
}

// these defaults seem to trigger the issue
public class ReaderOptions
{
    public int BatchSize { get; set; } = 100;
    public int ChunkSize { get; set; } = 4_000_000;
    public TimeSpan Delay { get; set; } = TimeSpan.FromMilliseconds(15);
    public bool FillBuffer { get; set; } = true;
    public StreamSource StreamSource { get; set; } = StreamSource.Http;
}

public enum StreamSource
{
    Http,
    Local
}