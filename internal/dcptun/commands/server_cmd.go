/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	cmdutil "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/dcptun"
	"github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
)

func NewRunServerCommand(log logr.Logger) *cobra.Command {
	runServerCmd := &cobra.Command{
		Use:   "server [--server-control-address address] [--server-control-port port] " + securityFlagsUsage + " client-control-address client-control-port client-data-address client-data-port",
		Short: "Runs the server-side proxy of the DCP tunnel",
		Long: `Runs the server-side proxy of the DCP tunnel.

		Required arguments are the address and port for both the control endpoint and the data endpoint of the client-side tunnel proxy.
		Optional arguments are the address and port for the control endpoint of the server-side proxy. If not set, "localhost" will be used for the address and a random port will be used for the port.
		`,
		RunE: runServerProxy(log),
		Args: cobra.ExactArgs(4),
	}

	runServerCmd.Flags().StringVar(&tunnelConfig.ServerControlAddress, "server-control-address", "localhost", "The address the server-side tunnel proxy should listen on for its control endpoint. Defaults to localhost.")
	runServerCmd.Flags().Int32Var(&tunnelConfig.ServerControlPort, "server-control-port", 0, "The port the server-side tunnel proxy should listen on for its control endpoint. If not specified, a random port will be used.")
	addSecurityFlags(runServerCmd)

	cmdutil.AddMonitorFlags(runServerCmd)

	return runServerCmd
}

func runServerProxy(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("server")

		configErr := ensureServerConfig(args)
		if configErr != nil {
			log.Error(configErr, "Invocation parameters are invalid")
			return configErr
		}

		serverProxyCtx, cancelServerProxyCtx := cmdutil.GetMonitorContextFromFlags(cmd.Context(), log)
		defer cancelServerProxyCtx()

		var clientProxyConnOpts []grpc.DialOption
		if tunnelConfig.HasCompleteCertificateData() {
			clientCertPool, clientCertPoolErr := tunnelConfig.GetClientPool()
			if clientCertPoolErr != nil {
				log.Error(clientCertPoolErr, "Failed to create TLS client certificate pool")
				return clientCertPoolErr
			}

			clientProxyConnOpts = append(clientProxyConnOpts, grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(clientCertPool, "")))
			log.V(1).Info("Using secure gRPC connection to client proxy")
		} else {
			clientProxyConnOpts = append(clientProxyConnOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
			log.V(1).Info("Using insecure gRPC connection to client proxy")
		}

		clientProxyConn, clientProxyErr := grpc.NewClient(
			networking.AddressAndPort(tunnelConfig.ClientControlAddress, tunnelConfig.ClientControlPort),
			clientProxyConnOpts...,
		)
		if clientProxyErr != nil {
			log.Error(clientProxyErr, "Failed to connect to the client-side proxy",
				"Address", tunnelConfig.ClientControlAddress,
				"Port", tunnelConfig.ClientControlPort,
			)
			return clientProxyErr
		}
		defer clientProxyConn.Close()

		lc := net.ListenConfig{}
		ctrlListener, ctrlListenerErr := lc.Listen(serverProxyCtx, "tcp", networking.AddressAndPort(tunnelConfig.ServerControlAddress, tunnelConfig.ServerControlPort))
		if ctrlListenerErr != nil {
			log.Error(ctrlListenerErr, "Failed to create TCP listener for server-side tunnel proxy",
				"Address", tunnelConfig.ServerControlAddress,
				"Port", tunnelConfig.ServerControlPort,
			)
			return ctrlListenerErr
		}
		defer func() { _ = ctrlListener.Close() }()

		ctrlListenerAddr := ctrlListener.Addr().(*net.TCPAddr)
		tunnelConfig.ServerControlAddress = ctrlListenerAddr.IP.String()
		tunnelConfig.ServerControlPort = int32(ctrlListenerAddr.Port)

		configJson, marshalErr := json.Marshal(tunnelConfig)
		if marshalErr != nil {
			log.Error(marshalErr, "Failed to create server proxy status message") // Should never happen
			return marshalErr
		}

		clientProxy := proto.NewTunnelControlClient(clientProxyConn)
		serverProxy := dcptun.NewServerProxy(
			serverProxyCtx,
			clientProxy,
			tunnelConfig.ClientDataAddress,
			tunnelConfig.ClientDataPort,
			cancelServerProxyCtx,
			log,
		)

		var grpcServerOpts []grpc.ServerOption
		if tunnelConfig.HasCompleteCertificateData() {
			tlsConfig, tlsConfigErr := tunnelConfig.GetTlsConfig()
			if tlsConfigErr != nil {
				log.Error(tlsConfigErr, "Failed to create TLS configuration")
				return tlsConfigErr
			}

			grpcServerOpts = append(grpcServerOpts, grpc.Creds(credentials.NewTLS(tlsConfig)))
			log.V(1).Info("Using secure gRPC server for control endpoint")
		} else {
			log.V(1).Info("Using insecure gRPC server for control endpoint")
		}

		controlEndpointServer := grpc.NewServer(grpcServerOpts...)
		proto.RegisterTunnelControlServer(controlEndpointServer, serverProxy)

		grpcServerErrChan := make(chan error, 1)
		go func() {
			serveErr := controlEndpointServer.Serve(ctrlListener)
			if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
				log.Error(serveErr, "Server-side tunnel proxy control endpoint encountered an error")
				grpcServerErrChan <- serveErr
			}
		}()

		log.V(1).Info("Server-side tunnel proxy is listening", "Config", configJson)
		fmt.Fprintln(os.Stdout, string(configJson))

		select {

		case <-serverProxyCtx.Done():
			log.Info("Server-side tunnel proxy is shutting down...")

			// Do not use GracefulStop() here because it takes and hols the same lock that grpcServer.Stop() does.
			select {
			case <-serverProxy.Done():
				// Shut down gracefully.
			case <-time.After(gracefulShutdownTimeout):
				log.Error(errors.New("failed to gracefully stop the server-side tunnel proxy within the timeout period"), "Graceful shutddown timed out")
				controlEndpointServer.Stop()
			}

			return nil

		case serveErr := <-grpcServerErrChan:
			return serveErr

		}
	}
}

func ensureServerConfig(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("expected exactly four arguments: client-control-address, client-control-port, client-data-address and client-data-port, but got %d arguments instead", len(args))
	}

	if len(args[0]) == 0 {
		return fmt.Errorf("client proxy control address must not be empty")
	}
	tunnelConfig.ClientControlAddress = args[0]

	portVal, portErr := strconv.ParseInt(args[1], 10, 32)
	if portErr != nil {
		return fmt.Errorf("client proxy control port must be a valid integer, got %s: %w", args[1], portErr)
	}
	if !networking.IsValidPort(int(portVal)) {
		return fmt.Errorf("client proxy control port must be a valid port number (1-65535), not %d", portVal)
	}
	tunnelConfig.ClientControlPort = int32(portVal)

	if len(args[2]) == 0 {
		return fmt.Errorf("client proxy data address must not be empty")
	}
	tunnelConfig.ClientDataAddress = args[2]

	portVal, portErr = strconv.ParseInt(args[3], 10, 32)
	if portErr != nil {
		return fmt.Errorf("client proxy data port must be a valid integer, got %s: %w", args[3], portErr)
	}
	if !networking.IsValidPort(int(portVal)) {
		return fmt.Errorf("client proxy data port must be a valid port number (1-65535), not %d", portVal)
	}
	tunnelConfig.ClientDataPort = int32(portVal)

	if len(tunnelConfig.ServerControlAddress) == 0 {
		return fmt.Errorf("server proxy control address must not be empty")
	}

	if !networking.IsBindablePort(int(tunnelConfig.ServerControlPort)) {
		return fmt.Errorf("server proxy port must be a valid port number (1-65535) or 0, not %d", tunnelConfig.ServerControlPort)
	}

	return validateSecurityFlagValues()
}
