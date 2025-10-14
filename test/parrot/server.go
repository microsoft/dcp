// Copyright (c) Microsoft Corporation. All rights reserved.

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run parrot in server mode",
	Long:  `Server mode listens for incoming TCP connections and echoes back received lines.`,
	RunE:  runServer,
}

var (
	serverAddress       string
	serverPort          string
	serverConversations int
	serverTimeout       time.Duration
)

func init() {
	rootCmd.AddCommand(serverCmd)
	serverCmd.Flags().StringVar(&serverAddress, "address", "127.0.0.1", "Address to bind to")
	serverCmd.Flags().StringVar(&serverPort, "port", "8080", "Port to bind to")
	serverCmd.Flags().IntVar(&serverConversations, "conversations", 10, "Number of conversations (line exchanges) before exiting")
	serverCmd.Flags().DurationVar(&serverTimeout, "timeout", 30*time.Second, "Timeout duration")
}

func runServer(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serverTimeout)
	defer cancel()

	listenAddr := net.JoinHostPort(serverAddress, serverPort)
	listener, listenerErr := net.Listen("tcp", listenAddr)
	if listenerErr != nil {
		return fmt.Errorf("failed to listen on %s: %w", listenAddr, listenerErr)
	}
	defer listener.Close()

	// Set up a channel to receive connection
	connChan := make(chan net.Conn, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errChan <- err
			return
		}
		connChan <- conn
	}()

	var conn net.Conn
	select {
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for connection")
	case err := <-errChan:
		return fmt.Errorf("failed to accept connection: %w", err)
	case conn = <-connChan:
		defer conn.Close()
	}

	// Handle conversations
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	for i := 0; i < serverConversations; i++ {
		// Set read deadline for each conversation
		if err := conn.SetReadDeadline(time.Now().Add(serverTimeout)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		line, lineErr := reader.ReadString('\n')
		if lineErr != nil {
			return fmt.Errorf("failed to read line %d: %w", i+1, lineErr)
		}

		// Set write deadline
		if err := conn.SetWriteDeadline(time.Now().Add(serverTimeout)); err != nil {
			return fmt.Errorf("failed to set write deadline: %w", err)
		}

		// Echo back the same line
		if _, err := writer.WriteString(line); err != nil {
			return fmt.Errorf("failed to write line %d: %w", i+1, err)
		}

		if err := writer.Flush(); err != nil {
			return fmt.Errorf("failed to flush line %d: %w", i+1, err)
		}
	}

	fmt.Fprintf(os.Stdout, "Successfully completed %d conversations, exiting"+string(osutil.LineSep()), serverConversations)

	return nil
}
