package apiserver

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	serverbuilder "github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/start"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/pkg/kubeconfig"
	stdtypes_apiv1 "github.com/usvc-dev/stdtypes/api/v1"
	stdtypes_openapi "github.com/usvc-dev/stdtypes/pkg/generated/openapi"
)

const (
	msgApiServerStartupFailed = "API server could not be started"
	dataFolderPath            = "data" // Not really used for in-memory storage, but needs to be consistent between CRDs.
	invalidPort               = -1
)

type ApiServer struct {
	name         string
	flushLogger  func()
	runCompleted bool
}

func NewApiServer(name string, flushLogger func()) *ApiServer {
	return &ApiServer{
		name:         name,
		flushLogger:  flushLogger,
		runCompleted: false,
	}
}

func (s *ApiServer) Name() string {
	return s.name
}

func (s *ApiServer) Run(ctx context.Context) error {
	log := runtimelog.Log.WithName(s.name)
	defer s.flushLogger()

	log.Info("Starting API server...")

	if s.runCompleted {
		err := fmt.Errorf("API server has already been run")
		log.Error(err, msgApiServerStartupFailed)
		return err
	}
	defer func() {
		s.runCompleted = true
	}()

	// The two constants below are just metadata for Swagger UI
	const openApiConfigrationName = "DCP"
	const openApiConfigurationVersion = "1.0.0" // TODO: use DCP executable version

	// Add well-known DCP types to API server metadata.
	builder := serverbuilder.NewServerBuilder().
		WithResourceMemoryStorage(&stdtypes_apiv1.Executable{}, dataFolderPath).
		WithResourceMemoryStorage(&stdtypes_apiv1.Container{}, dataFolderPath).
		WithResourceMemoryStorage(&stdtypes_apiv1.ContainerVolume{}, dataFolderPath).
		WithOpenAPIDefinitions(openApiConfigrationName, openApiConfigurationVersion, stdtypes_openapi.GetOpenAPIDefinitions)

	options, err := computeServerOptions(builder, log)
	if err != nil {
		return err
	}

	stoppedCh, err := options.RunTiltServer(ctx)
	if err != nil {
		log.Error(err, "API server execution error")
		return err
	}

	log.Info("API server started")

	<-stoppedCh
	log.Info("API server shut down")
	return nil
}

func computeServerOptions(builder *serverbuilder.Server, log logr.Logger) (*start.TiltServerOptions, error) {
	options, err := builder.ToServerOptions()
	if err != nil {
		err = fmt.Errorf("unable to create API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	fs := pflag.NewFlagSet("DCP API server", pflag.ContinueOnError)
	fs.ParseErrorsWhitelist.UnknownFlags = true
	fs.AddGoFlagSet(flag.CommandLine) // Adds flags defined by standard K8s packages, which put them in flag.CommandLine flag set.
	options.ServingOptions.AddFlags(fs)
	err = fs.Parse(os.Args[1:])
	if err != nil {
		err = fmt.Errorf("invalid API server invocation options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	// If --secure-port and/or --token were not specified, figure them out from Kubeconfig file
	havePort := isValidPort(options.ServingOptions.BindPort)
	haveToken := options.ServingOptions.BearerToken != ""
	if !havePort || !haveToken {
		kubeconfigPath, err := kubeconfig.EnsureKubeconfigFile(fs)
		if err != nil {
			log.Error(err, msgApiServerStartupFailed)
			return nil, err
		}

		port, token, err := kubeconfig.GetKubeConfigData(kubeconfigPath)
		if err != nil {
			err = fmt.Errorf("could not obtain port and security token information from Kubeconfig file: %w", err)
			log.Error(err, msgApiServerStartupFailed)
			return nil, err
		}

		if !havePort {
			options.ServingOptions.BindPort = port
		}

		if !haveToken {
			options.ServingOptions.BearerToken = token
		}
	}

	err = options.Validate(nil)
	if err != nil {
		err = fmt.Errorf("unable to validate API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	return options, nil
}

func isValidPort(port int) bool {
	return port >= 1 && port <= 65535
}
