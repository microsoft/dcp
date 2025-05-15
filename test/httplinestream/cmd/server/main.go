package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/test/httplinestream/pkg/web"
)

func main() {
	// Parse command line flags
	var (
		addr = flag.Int("addr", 8080, "HTTP server address")
	)
	flag.Parse()

	// Set up logging
	log := logger.New("server")

	// Create server
	server := web.NewServer(*addr, &log.Logger)

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("Received shutdown signal")

		// Create a timeout context for shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "failed to shut down server gracefully")
		}
	}()

	// Start the server
	log.Info("Starting server")
	if err := server.Start(); err != nil {
		log.Error(err, "server failed")
	}
}
