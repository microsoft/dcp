/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Run parrot in client mode",
	Long:  `Client mode connects to a TCP server and sends lines, verifying that the server echoes them back.`,
	RunE:  runClient,
}

var (
	clientAddress       string
	clientPort          string
	clientConversations int
	clientTimeout       time.Duration
)

func init() {
	rootCmd.AddCommand(clientCmd)
	clientCmd.Flags().StringVar(&clientAddress, "address", "127.0.0.1", "Address to connect to")
	clientCmd.Flags().StringVar(&clientPort, "port", "8080", "Port to connect to")
	clientCmd.Flags().IntVar(&clientConversations, "conversations", 10, "Number of conversations (line exchanges) to perform")
	clientCmd.Flags().DurationVar(&clientTimeout, "timeout", 30*time.Second, "Timeout duration")
}

func runClient(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()

	serverAddr := net.JoinHostPort(clientAddress, clientPort)

	// Retry connection until success or timeout
	var conn net.Conn
	retryInterval := 100 * time.Millisecond
	maxRetryInterval := 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout while trying to connect to %s", serverAddr)
		default:
		}

		var err error
		conn, err = net.DialTimeout("tcp", serverAddr, retryInterval)
		if err == nil {
			fmt.Fprintf(os.Stdout, "Connected to server at %s (time %s)\n", serverAddr, time.Now().Format(time.TimeOnly))
			os.Stdout.Sync()
			break
		}

		// Exponential backoff
		fmt.Fprintf(os.Stdout, "Attempt to connect to the server failed, will retry in %s. Time now %s\n", retryInterval.String(), time.Now().Format(time.TimeOnly))
		os.Stdout.Sync()
		time.Sleep(retryInterval)
		retryInterval *= 2
		if retryInterval > maxRetryInterval {
			retryInterval = maxRetryInterval
		}
	}
	defer conn.Close()

	// Handle conversations
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	for i := 0; i < clientConversations; i++ {
		// Check if context is still valid
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout during conversation %d", i+1)
		default:
		}

		// Prepare test line
		testLine := fmt.Sprintf("I am telling you: %d is an important number!\n", i+1)

		// Set write deadline
		if err := conn.SetWriteDeadline(time.Now().Add(clientTimeout)); err != nil {
			return fmt.Errorf("failed to set write deadline: %w", err)
		}

		// Send the line
		if _, err := writer.WriteString(testLine); err != nil {
			return fmt.Errorf("failed to write line %d: %w", i+1, err)
		}

		if err := writer.Flush(); err != nil {
			return fmt.Errorf("failed to flush line %d: %w", i+1, err)
		}

		// Set read deadline
		if err := conn.SetReadDeadline(time.Now().Add(clientTimeout)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		// Read the response
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read response for line %d: %w", i+1, err)
		}

		// Verify the response matches what was sent
		if response != testLine {
			return fmt.Errorf("conversation %d: expected %q, got %q", i+1, testLine, response)
		}
	}

	fmt.Fprintf(os.Stdout, "Successfully completed %d conversations, exiting\n", clientConversations)

	return nil
}
