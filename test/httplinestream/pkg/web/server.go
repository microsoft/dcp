package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/test/httplinestream/pkg/line"
)

// Server implements an HTTP server that streams lines
type Server struct {
	srv    *http.Server
	logger *logr.Logger
}

// NewServer creates a new HTTP server
func NewServer(addr int, logger *logr.Logger) *Server {
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", addr),
		Handler: mux,
	}

	server := &Server{
		srv:    srv,
		logger: logger,
	}

	// Set up endpoints
	mux.HandleFunc("/values.csv", server.handleValuesCSV)

	return server
}

// handleValuesCSV responds with a stream of lines
func (s *Server) handleValuesCSV(w http.ResponseWriter, r *http.Request) {
	options := line.NewWebWriteOptions(s.logger)

	w.Header().Set("Content-Type", "text/csv;charset=utf-8")
	w.WriteHeader(http.StatusOK)

	done := r.Context().Done()
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Write lines to the response
	line.WriteLinesAsync(w, options, done)
}

// Start starts the HTTP server
func (s *Server) Start() error {
	s.logger.Info("Starting HTTP server", "addr", s.srv.Addr)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Shutting down HTTP server")
	return s.srv.Shutdown(ctx)
}
