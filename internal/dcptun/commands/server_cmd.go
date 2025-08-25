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
	"google.golang.org/grpc/credentials/insecure"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	"github.com/microsoft/usvc-apiserver/internal/dcptun/proto"
	"github.com/microsoft/usvc-apiserver/internal/networking"
)

func NewRunServerCommand(log logr.Logger) *cobra.Command {
	runServerCmd := &cobra.Command{
		Use:   "server [--server-control-address address] [--server-control-port port] client-control-address client-control-port client-data-address client-data-port",
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

		clientProxyConn, clientProxyErr := grpc.NewClient(
			networking.AddressAndPort(tunnelConfig.ClientControlAddress, tunnelConfig.ClientControlPort),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
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
		controlEndpointServer := grpc.NewServer()
		proto.RegisterTunnelControlServer(controlEndpointServer, serverProxy)

		grpcServerErrChan := make(chan error, 1)
		go func() {
			serveErr := controlEndpointServer.Serve(ctrlListener)
			if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
				log.Error(serveErr, "Server-side tunnel proxy control endpoint encountered an error")
				grpcServerErrChan <- serveErr
			}
		}()

		log.V(1).Info("Server-side tunnel proxy is listening", "config", configJson)
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
		return fmt.Errorf("Expected exactly four arguments: client-control-address, client-control-port, client-data-address and client-data-port, but got %d arguments instead", len(args))
	}

	if len(args[0]) == 0 {
		return fmt.Errorf("Client proxy control address must not be empty")
	}
	tunnelConfig.ClientControlAddress = args[0]

	portVal, portErr := strconv.ParseInt(args[1], 10, 32)
	if portErr != nil {
		return fmt.Errorf("Client proxy control port must be a valid integer, got %s: %w", args[1], portErr)
	}
	if !networking.IsValidPort(int(portVal)) {
		return fmt.Errorf("Client proxy control port must be a valid port number (1-65535), not %d", portVal)
	}
	tunnelConfig.ClientControlPort = int32(portVal)

	if len(args[2]) == 0 {
		return fmt.Errorf("Client proxy data address must not be empty")
	}
	tunnelConfig.ClientDataAddress = args[2]

	portVal, portErr = strconv.ParseInt(args[3], 10, 32)
	if portErr != nil {
		return fmt.Errorf("Client proxy data port must be a valid integer, got %s: %w", args[3], portErr)
	}
	if !networking.IsValidPort(int(portVal)) {
		return fmt.Errorf("Client proxy data port must be a valid port number (1-65535), not %d", portVal)
	}
	tunnelConfig.ClientDataPort = int32(portVal)

	if len(tunnelConfig.ServerControlAddress) == 0 {
		return fmt.Errorf("Server proxy control address must not be empty")
	}

	if !networking.IsBindablePort(int(tunnelConfig.ServerControlPort)) {
		return fmt.Errorf("Server proxy port must be a valid port number (1-65535) or 0, not %d", tunnelConfig.ServerControlPort)
	}

	return nil
}
