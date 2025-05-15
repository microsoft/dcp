using System.Buffers.Text;
using System.Text;
using Microsoft.Extensions.Logging;

namespace HttpContentStreamRepro.Client;

public static class Line
{
    public const int MaxLineLength = 128;
    private const int TicksPerMs = 10_000;
    private const int TicksPerSec = 1_000 * TicksPerMs;

    // $"abc,{row},def,{new string((char)(row % 26 + (byte)'A'), row % 13 + 1)},ghi,{GetTime1(row)},jkl,{GetTime2(row)},mno"
    public static int FormatLine(Span<byte> buffer, long row, bool eol)
    {
        var c = (byte)(row % 26 + (byte)'A');
        var written = Encoding.UTF8.GetBytes("abc,", buffer);
        Utf8Formatter.TryFormat(row, buffer[written..], out var bytes);
        written += bytes;
        written += Encoding.UTF8.GetBytes(",def,", buffer[written..]);
        for (var i = 0; i <= row % 13; i++)
            buffer[written++] = c;
        written += Encoding.UTF8.GetBytes(",ghi,", buffer[written..]);
        Utf8Formatter.TryFormat(GetTime1(row), buffer[written..], out bytes);
        written += bytes;
        written += Encoding.UTF8.GetBytes(",jkl,", buffer[written..]);
        Utf8Formatter.TryFormat(GetTime2(row), buffer[written..], out bytes);
        written += bytes;
        written += Encoding.UTF8.GetBytes(",mno", buffer[written..]);

        if (eol)
            buffer[written++] = (byte)'\n';

        return written;
    }

    private static DateTimeOffset GetTime1(long row) => new(row * TicksPerMs, TimeSpan.Zero);
    private static DateTimeOffset GetTime2(long row) => new(row * TicksPerSec, TimeSpan.Zero);

    public static async Task WriteLinesAsync(Stream stream, WriteOptions options, CancellationToken cancellationToken)
    {
        var line = new byte[MaxLineLength];
        var row = 0L;
        while (row < options.MaxRows)
        {
            var length = FormatLine(line, row, eol: true);
            var split = (int)(row % (length / 2)) + 1;
            await stream.WriteAsync(line.AsMemory(0, split), cancellationToken).ConfigureAwait(false);
            await stream.WriteAsync(line.AsMemory(split, length - split), cancellationToken).ConfigureAwait(false);

            row++;

            if (row % options.RowsPerDelay == 0)
                await Task.Delay(options.Delay, cancellationToken);

            if (row % options.RowsPerLog == 0)
                options.Logger?.LogInformation("Wrote {Count:N0} lines", row);
        }

        if (stream is ReadWriteStream readWriteStream)
            readWriteStream.EndOfStream = true;
    }
}

public class WriteOptions(ILogger logger)
{
    public TimeSpan Delay { get; set; }
    public ILogger? Logger { get; } = logger;
    public long MaxRows { get; set; } = 20_000_000;
    public long? RowsPerDelay { get; set; }
    public long RowsPerLog { get; set; }

    public static WriteOptions Local(ILogger logger) => new(logger)
    {
        Delay = TimeSpan.FromMilliseconds(100),
        RowsPerDelay = 1_000,
        RowsPerLog = 50_000
    };

    public static WriteOptions Web(ILogger logger) => new(logger)
    {
        RowsPerLog = 10_000
    };
}