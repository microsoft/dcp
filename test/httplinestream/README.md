# HTTP Content Stream Reproduction in Go

This is a Go implementation of the HTTP content stream test that demonstrates potential issues with HTTP streaming.

It is based on the C# implementation found at https://github.com/mhenry07/http-content-stream-repro/tree/v1

## Overview

The project consists of two main components:

1. **Server**: Generates and streams lines of text via HTTP
2. **Client**: Retrieves the stream and validates each line

The implementation allows testing with different buffer strategies to identify potential issues with HTTP streaming.

## Project Structure

- `pkg/line`: Line generation and validation logic
- `pkg/reader`: Stream reading and processing
- `pkg/stream`: Custom stream implementation for local testing
- `pkg/web`: HTTP server implementation
- `cmd/client`: Client application
- `cmd/server`: Server application

## Building and Running

### Build both client and server

```bash
cd go-impl
go build ./cmd/client
go build ./cmd/server
```

### Running the Server

```bash
./server -addr=:8080
```

### Running the Client

```bash
./client -base-url=http://localhost:8080 -fill-buffer=true
```

## Configuration Options

### Client Options

- `batch-size`: Number of lines per batch (default: 100)
- `chunk-size`: Size of read buffer in bytes (default: 4,000,000)
- `delay`: Delay between batches (default: 15ms)
- `fill-buffer`: Whether to fill the buffer completely or read once (default: true)
- `stream-source`: Stream source (http or local) (default: http)
- `base-url`: Base URL for HTTP requests (default: http://localhost:8080)

### Server Options

- `addr`: HTTP server address (default: :8080)

## Using as Part of Go Tests

Both the client and server components are designed to be used as part of Go unit tests:

```go
import (
    "context"
    "net/http"
    "testing"
    "time"

    "github.com/dnegstad/http-content-stream-repro/pkg/reader"
    "github.com/dnegstad/http-content-stream-repro/pkg/web"
    "github.com/rs/zerolog"
)

func TestStreamProcessing(t *testing.T) {
    // Set up logging
    logger := zerolog.New(zerolog.NewTestWriter(t)).With().Timestamp().Logger()

    // Start server
    server := web.NewServer(":8081", &logger)
    go server.Start()
    defer server.Shutdown(context.Background())

    // Wait for server to start
    time.Sleep(100 * time.Millisecond)

    // Create client
    httpClient := &http.Client{Timeout: 0}
    options := reader.DefaultReaderOptions()
    r := reader.NewReader(httpClient, &logger, options, "http://localhost:8081")
    defer r.Close()

    // Get and process stream
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    stream, err := r.GetStream(ctx)
    if err != nil {
        t.Fatalf("Failed to get stream: %v", err)
    }
    defer stream.Close()

    err = r.ReadStream(ctx, stream)
    if err != nil {
        t.Fatalf("Failed to read stream: %v", err)
    }
}
```

## Reproducing the Issue

To reproduce the issue demonstrated in the C# implementation:

1. Start the server: `./server`
2. Run the client with buffer filling enabled: `./client -fill-buffer=true`
3. Compare with non-filling mode: `./client -fill-buffer=false`

The issue is more likely to appear when using the `fill-buffer=true` option, which continuously reads from the stream until the buffer is full, similar to the C# implementation.
