package apiserver

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/spf13/pflag"
	serverbuilder "github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	msgApiServerStartupFailed = "API server could not be started"
)

type ApiServer struct {
	name         string
	flushLogger  func()
	PortInfo     chan int
	runCompleted bool
}

func NewApiServer(name string, flushLogger func()) *ApiServer {
	return &ApiServer{
		name:         name,
		flushLogger:  flushLogger,
		PortInfo:     make(chan int, 1),
		runCompleted: false,
	}
}

func (s *ApiServer) Name() string {
	return s.name
}

func (s *ApiServer) Run(ctx context.Context) error {
	log := runtimelog.Log.WithName(s.name)
	defer s.flushLogger()

	if s.runCompleted {
		err := fmt.Errorf("API server has already been run")
		log.Error(err, msgApiServerStartupFailed)
		return err
	}

	defer func() {
		close(s.PortInfo)
		s.runCompleted = true
	}()

	builder := serverbuilder.NewServerBuilder()
	options, err := builder.ToServerOptions()
	if err != nil {
		err = fmt.Errorf("unable to create API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return err
	}

	// Run the API server
	fs := pflag.NewFlagSet("dcpd", pflag.ContinueOnError)
	fs.AddGoFlagSet(flag.CommandLine)
	options.ServingOptions.AddFlags(fs)
	err = fs.Parse(os.Args[1:])
	if err != nil {
		err = fmt.Errorf("invalid API server invocation options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return err
	}

	err = options.Validate(nil)
	if err != nil {
		err = fmt.Errorf("unable to validate API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return err
	}

	stoppedCh, err := options.RunTiltServer(ctx)
	if err != nil {
		log.Error(err, "API server execution error")
		return err
	}

	// Report the port we will be using
	s.PortInfo <- getPort(options.ServingOptions.Listener.Addr())

	<-stoppedCh
	return nil
}

func getPort(addr net.Addr) int {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return int(a.Port)
	case *net.TCPAddr:
		return int(a.Port)
	default:
		return 0
	}
}
