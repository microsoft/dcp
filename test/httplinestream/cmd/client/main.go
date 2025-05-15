package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/test/httplinestream/pkg/reader"
)

func main() {
	// Parse command line flags
	var (
		batchSize    = flag.Int("batch-size", 100, "Number of lines per batch")
		chunkSize    = flag.Int("chunk-size", 4_000_000, "Size of read buffer")
		delay        = flag.Duration("delay", 15*time.Millisecond, "Delay between batches")
		fillBuffer   = flag.Bool("fill-buffer", true, "Whether to fill the buffer")
		streamSource = flag.String("stream-source", "http", "Stream source (http or local)")
		baseURL      = flag.String("base-url", "http://localhost:8080", "Base URL for HTTP requests")
	)
	flag.Parse()

	// Set up logging
	log := logger.New("client")

	// Create HTTP client
	httpClient := &http.Client{
		Timeout: 0, // No timeout for streaming
	}

	// Create reader options
	options := &reader.ReaderOptions{
		BatchSize:    *batchSize,
		ChunkSize:    *chunkSize,
		Delay:        *delay,
		FillBuffer:   *fillBuffer,
		StreamSource: reader.StreamSourceHTTP,
	}

	// Set stream source
	if *streamSource == "local" {
		options.StreamSource = reader.StreamSourceLocal
	}

	// Create reader
	r := reader.NewReader(httpClient, &log.Logger, options, *baseURL)
	defer r.Close()

	// Set up context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Get and read stream
	stream, err := r.GetStream(ctx)
	if err != nil {
		log.Error(err, "failed to get stream")
		os.Exit(1)
	}
	defer stream.Close()

	// Read and process stream
	err = r.ReadStream(ctx, stream)
	if err != nil {
		log.Error(err, "failed to read stream")
		os.Exit(1)
	}

	log.Info("Stream processing completed successfully")
}
