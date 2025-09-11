package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	"github.com/microsoft/usvc-apiserver/internal/dcptun/proto"
	"github.com/microsoft/usvc-apiserver/internal/networking"
)

func NewRunClientCommand(log logr.Logger) *cobra.Command {
	runClientCmd := &cobra.Command{
		Use:   "client [--client-control-address addr] [--client-control-port port] [--client-data-address addr] [--client-data-port port]",
		Short: "Runs the client-side proxy of the DCP tunnel",
		Long: `Runs the client-side proxy of the DCP tunnel.

		Optional arguments are the address and port for both the control endpoint and the data endpoint of the client-side tunnel proxy.
		`,
		RunE: runClientProxy(log),
		Args: cobra.NoArgs,
	}

	runClientCmd.Flags().StringVar(&tunnelConfig.ClientControlAddress, "client-control-address", "localhost", "The address the client-side tunnel proxy should listen on for its control endpoint. Defaults to localhost.")
	runClientCmd.Flags().Int32Var(&tunnelConfig.ClientControlPort, "client-control-port", 0, "The port the client-side tunnel proxy should listen on for its control endpoint. If not specified, a random port will be used.")
	runClientCmd.Flags().StringVar(&tunnelConfig.ClientDataAddress, "client-data-address", "localhost", "The address the client-side tunnel proxy should listen on for its data endpoint. Defaults to localhost.")
	runClientCmd.Flags().Int32Var(&tunnelConfig.ClientDataPort, "client-data-port", 0, "The port the client-side tunnel proxy should listen on for its data endpoint. If not specified, a random port will be used.")

	return runClientCmd
}

func runClientProxy(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("client")

		configErr := ensureClientConfig()
		if configErr != nil {
			log.Error(configErr, "Invocation parameters are invalid")
			return configErr
		}

		clientProxyCtx, cancelClientProxyCtx := cmdutil.GetMonitorContextFromFlags(cmd.Context(), log)
		defer cancelClientProxyCtx()

		lc := net.ListenConfig{}
		ctrlListener, ctrlListenerErr := lc.Listen(clientProxyCtx, "tcp", networking.AddressAndPort(tunnelConfig.ClientControlAddress, tunnelConfig.ClientControlPort))
		if ctrlListenerErr != nil {
			log.Error(ctrlListenerErr, "Failed to create TCP listener for the control endpoint of the client-side tunnel proxy",
				"Address", tunnelConfig.ClientControlAddress,
				"Port", tunnelConfig.ClientControlPort,
			)
			return ctrlListenerErr
		}
		defer func() { _ = ctrlListener.Close() }()

		ctrlListenerAddr := ctrlListener.Addr().(*net.TCPAddr)
		tunnelConfig.ClientControlAddress = ctrlListenerAddr.IP.String()
		tunnelConfig.ClientControlPort = int32(ctrlListenerAddr.Port)

		dataListener, dataListenerErr := lc.Listen(clientProxyCtx, "tcp", networking.AddressAndPort(tunnelConfig.ClientDataAddress, tunnelConfig.ClientDataPort))
		if dataListenerErr != nil {
			log.Error(dataListenerErr, "Failed to create TCP listener for the data endpoint of the client-side tunnel proxy",
				"Address", tunnelConfig.ClientDataAddress,
				"Port", tunnelConfig.ClientDataPort,
			)
			return dataListenerErr
		}
		defer func() { _ = dataListener.Close() }()

		dataListenerAddr := dataListener.Addr().(*net.TCPAddr)
		tunnelConfig.ClientDataAddress = dataListenerAddr.IP.String()
		tunnelConfig.ClientDataPort = int32(dataListenerAddr.Port)

		configJson, marshalErr := json.Marshal(tunnelConfig)
		if marshalErr != nil {
			log.Error(marshalErr, "Failed to create server proxy status message") // Should never happen
			return marshalErr
		}

		clientProxy := dcptun.NewClientProxy(clientProxyCtx, dataListener, cancelClientProxyCtx, log)
		controlEndpointServer := grpc.NewServer()
		proto.RegisterTunnelControlServer(controlEndpointServer, clientProxy)

		grpcServerErrChan := make(chan error, 1)
		go func() {
			serveErr := controlEndpointServer.Serve(ctrlListener)
			if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
				log.Error(serveErr, "Client-side tunnel proxy control endpoint encountered an error")
				grpcServerErrChan <- serveErr
			}
		}()

		log.V(1).Info("Client-side tunnel proxy is listening", "Config", configJson)
		fmt.Fprintln(os.Stdout, string(configJson))

		select {

		case <-clientProxyCtx.Done():
			log.Info("Client-side tunnel proxy is shutting down...")

			// Do not use GracefulStop() here because it takes and hols the same lock that grpcServer.Stop() does.
			select {
			case <-clientProxy.Done():
				// Shut down gracefully.
			case <-time.After(gracefulShutdownTimeout):
				log.Error(nil, "Failed to gracefully stop the client-side tunnel proxy control endpoint within the timeout period", "Graceful shutdown timed out")
				controlEndpointServer.Stop()
			}

			return nil

		case serveErr := <-grpcServerErrChan:
			cancelClientProxyCtx()
			return serveErr
		}
	}
}

func ensureClientConfig() error {
	if len(tunnelConfig.ClientControlAddress) == 0 {
		return fmt.Errorf("client proxy control address must not be empty")
	}

	if !networking.IsValidPort(int(tunnelConfig.ClientControlPort)) {
		return fmt.Errorf("client proxy control port must be a valid port number (1-65535), not %d", tunnelConfig.ClientControlPort)
	}

	if len(tunnelConfig.ClientDataAddress) == 0 {
		return fmt.Errorf("client proxy data address must not be empty")
	}

	if !networking.IsValidPort(int(tunnelConfig.ClientDataPort)) {
		return fmt.Errorf("client proxy data port must be a valid port number (1-65535), not %d", tunnelConfig.ClientDataPort)
	}

	return nil
}
