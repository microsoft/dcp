// Copyright (c) Microsoft Corporation. All rights reserved.

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	kubeserverfilters "k8s.io/apiserver/pkg/server/filters"
	clientgorest "k8s.io/client-go/rest"

	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	tiltserverbuilder "github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	tiltresource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	tiltstart "github.com/tilt-dev/tilt-apiserver/pkg/server/start"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/logs/containerlogs"
	"github.com/microsoft/usvc-apiserver/internal/logs/stdiologs"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/generated/openapi"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	msgApiServerStartupFailed          = "API server could not be started"
	dataFolderPath                     = "data" // Not really used for in-memory storage, but needs to be consistent between CRDs.
	invalidPort                        = -1
	DCP_RESOURCE_WATCH_TIMEOUT_SECONDS = "DCP_RESOURCE_WATCH_TIMEOUT_SECONDS"
)

type ApiServer struct {
	name         string
	config       *kubeconfig.Kubeconfig
	logger       logr.Logger
	runCompleted bool
	builder      *tiltserverbuilder.Server
	options      *tiltstart.TiltServerOptions
	// Types that have data stored in the API server.
	persistentDcpTypes []tiltresource.Object
}

func NewApiServer(name string, config *kubeconfig.Kubeconfig, logger logr.Logger) *ApiServer {
	apiServer := &ApiServer{
		name:         name,
		config:       config,
		logger:       logger,
		runCompleted: false,
		builder:      tiltserverbuilder.NewServerBuilder(),
		persistentDcpTypes: []tiltresource.Object{
			&apiv1.Executable{},
			&apiv1.Endpoint{},
			&apiv1.ExecutableReplicaSet{},
			&apiv1.Container{},
			&apiv1.ContainerVolume{},
			&apiv1.ContainerNetwork{},
			&apiv1.ContainerNetworkConnection{},
			&apiv1.ContainerExec{},
			&apiv1.Service{},
		},
	}

	// The two constants below are just metadata for Swagger UI
	const openApiConfigrationName = "DCP"
	var openApiConfigurationVersion = version.Version().Version

	// Types that must be recognizable by the API server, but are not persisted
	// (they are used for request processing only).
	var additionalDcpTypes = []tiltresource.Object{
		&apiv1.LogOptions{},
		&apiv1.LogStreamer{},
	}

	for _, o := range apiServer.persistentDcpTypes {
		apiServer.builder = apiServer.builder.WithResourceMemoryStorage(o, dataFolderPath)
	}
	for _, o := range additionalDcpTypes {
		apiServer.builder = apiServer.builder.WithResource(o)
	}
	apiServer.builder = apiServer.builder.WithOpenAPIDefinitions(openApiConfigrationName, openApiConfigurationVersion, openapi.GetOpenAPIDefinitions)

	return apiServer
}

func (s *ApiServer) Options() (*tiltstart.TiltServerOptions, error) {
	if s.options != nil {
		return s.options, nil
	}

	options, err := s.builder.ToServerOptions()
	s.options = options

	return s.options, err
}

func (s *ApiServer) Name() string {
	return s.name
}

// Runs the API server.
//
// The passed runCtx is used to control the lifecycle of the API server. When that context is cancelled,
// the API server should terminate all requests in progress and shut down gracefully.
//
// shutdownRequested is a function that can be called to indicate that the API server was requested to shut down.
// It should NOT be assumed that this is a cancellation function for the runCtx (cancellation of the runCtx will happen
// shortly after, but ASYNCHRONOUSLY to the call to shutdownRequested).
func (s *ApiServer) Run(runCtx context.Context, shutdownRequested func(ApiServerShutdownResourceCleanup)) (<-chan struct{}, error) {
	log := s.logger.WithName(s.name)

	log.Info("Starting API server...")

	if s.runCompleted {
		err := fmt.Errorf("API server has already been run")
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}
	defer func() {
		s.runCompleted = true
	}()

	options, err := s.computeServerOptions(log)
	if err != nil {
		return nil, err
	}

	config, err := options.Config()
	if err != nil {
		err = fmt.Errorf("unable to create API server configuration: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	watchTimeout, watchTimeoutProvided := osutil.EnvVarIntVal(DCP_RESOURCE_WATCH_TIMEOUT_SECONDS)
	if watchTimeoutProvided && watchTimeout > 0 {
		// The K8s config option is called MinRequestTimeout, but it really only applies to watch requests.
		config.GenericConfig.MinRequestTimeout = watchTimeout
	}

	addDcpHttpHandlers(config, runCtx, log)

	err = configureForLogServing(config, s.persistentDcpTypes, log)
	if err != nil {
		return nil, err
	}

	// Save the kubeconfig file so that clients can use it to connect to the API server.
	err = s.config.Save()
	if err != nil {
		return nil, err
	}

	completedConfig := config.Complete()
	stoppedCh, err := runServerFromCompletedConfig(completedConfig, runCtx, shutdownRequested, log)
	if err != nil {
		log.Error(err, "API server execution error")
		return nil, err
	}

	log.Info("API server started", "Address", options.ServingOptions.BindAddress.String(), "Port", options.ServingOptions.BindPort)

	return stoppedCh, nil
}

func (s *ApiServer) Dispose() error {
	var allErrors error
	apiv1.ResourceLogStreamers.Range(func(_ schema.GroupVersionResource, rls apiv1.ResourceLogStreamer) bool {
		allErrors = errors.Join(allErrors, rls.Dispose())
		return true // Continue iteration
	})

	// Remove the kubeconfig file (best effort)
	_ = os.Remove(s.config.Path())

	return allErrors
}

func (s *ApiServer) computeServerOptions(log logr.Logger) (*tiltstart.TiltServerOptions, error) {
	options, err := s.Options()
	if err != nil {
		err = fmt.Errorf("unable to create API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	fs := pflag.NewFlagSet("DCP API server", pflag.ContinueOnError)
	fs.ParseErrorsWhitelist.UnknownFlags = true
	fs.AddGoFlagSet(flag.CommandLine) // Adds flags defined by standard K8s packages, which put them in flag.CommandLine flag set.
	err = fs.Parse(os.Args[1:])
	if err != nil {
		err = fmt.Errorf("invalid API server invocation options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	address, port, token, certificateData, err := s.config.GetData()
	if err != nil {
		err = fmt.Errorf("could not obtain address, port and security token information from Kubeconfig file: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	options.ServingOptions.BindAddress = address

	configuredToken, _ := os.LookupEnv(kubeconfig.DCP_SECURE_TOKEN)
	if configuredToken != "" {
		// If a token was supplied, use it to secure the API server
		options.ServingOptions.BearerToken = configuredToken
	}

	// If --secure-port and/or DCP_SECURE_TOKEN were not specified, figure them out from Kubeconfig file
	havePort := networking.IsValidPort(options.ServingOptions.BindPort)
	haveToken := options.ServingOptions.BearerToken != ""
	if !havePort || !haveToken {
		if !havePort {
			options.ServingOptions.BindPort = port
		}

		if !haveToken {
			if token == "" || token == kubeconfig.PlaceholderToken {
				// If DCP_SECURE_TOKEN isn't provided, the kubeconfig needs to include one.
				return nil, fmt.Errorf("kubeconfig file is invalid; an auth token was expected, but no valid token was provided")
			}

			options.ServingOptions.BearerToken = token
		}
	}

	if certificateData != nil {
		cert, certErr := certificateData.Certificate()
		if certErr != nil {
			return nil, fmt.Errorf("unable to obtain certificate data: %w", certErr)
		}
		key, keyErr := certificateData.Key()
		if keyErr != nil {
			return nil, fmt.Errorf("unable to obtain key data: %w", keyErr)
		}
		options.ServingOptions.ServerCert.GeneratedCert, err = dynamiccertificates.NewStaticCertKeyContent("DCP self signed certificate", cert, key)
		if err != nil {
			return nil, fmt.Errorf("unable to create static certificate: %w", err)
		}
		options.ServingOptions.ServerCert.PregeneratedCert = true
	}

	err = options.Validate(nil)
	if err != nil {
		err = fmt.Errorf("unable to validate API server options: %w", err)
		log.Error(err, msgApiServerStartupFailed)
		return nil, err
	}

	return options, nil
}

// Configures various API serving options so that requests for logs are handled correctly,
// including body and options serialization/deserialization and marking long requests
// as long-running, so they do not time out prematurely.
func configureForLogServing(config *tiltapiserver.Config, persistentDcpTypes []tiltresource.Object, log logr.Logger) error {
	// The following is necessary for the API server to correctly deserialize log request parameters into LogOptions instance.
	// (the scheme in ExtraConfig contains DCP type definitions, including LogOptions).
	disableOpenApiForLogsSubresource(config.GenericConfig, persistentDcpTypes)

	err := apiv1.RegisterLogOptionsConversions(config.ExtraConfig.Scheme)
	if err != nil {
		return err
	}
	config.ExtraConfig.ParameterCodec = apiruntime.NewParameterCodec(config.ExtraConfig.Scheme)

	config.GenericConfig.LongRunningFunc = kubeserverfilters.BasicLongRunningRequestCheck(
		sets.NewString("watch"),
		sets.NewString(apiv1.LogSubresourceName),
	)

	apiv1.ResourceLogStreamers.Store(
		(&apiv1.Executable{}).GetGroupVersionResource(),
		stdiologs.LogStreamer(),
	)
	apiv1.ResourceLogStreamers.Store(
		(&apiv1.Container{}).GetGroupVersionResource(),
		containerlogs.NewLogStreamer(log.WithName("container-logstreamer")),
	)
	apiv1.ResourceLogStreamers.Store(
		(&apiv1.ContainerExec{}).GetGroupVersionResource(),
		stdiologs.LogStreamer(),
	)

	return nil
}

// By default the API server expects that subresources have a structure, but logs do not have any,
// they are just a stream of text. We need to tell the API server that OpenAPI definitions are not available for logs.
func disableOpenApiForLogsSubresource(config *kubeapiserver.RecommendedConfig, objects []tiltresource.Object) {
	objectsWithLogs := slices.Select(objects, func(o tiltresource.Object) bool {
		owgs, hasSubresource := o.(tiltresource.ObjectWithGenericSubResource)
		if !hasSubresource {
			return false
		}

		hasLogs := slices.Any(owgs.GenericSubResources(), func(sr tiltresource.GenericSubResource) bool {
			return sr.Name() == apiv1.LogSubresourceName
		})
		return hasLogs
	})

	for _, o := range objectsWithLogs {
		if config.OpenAPIConfig == nil {
			panic("OpenAPIConfig should be set at this point, did you forget to call github.com/tilt-dev/tilt-apiserver/pkg/server/builder.Server.WithOpenAPIDefinitions()?")
		}

		gvr := o.GetGroupVersionResource()
		config.OpenAPIConfig.IgnorePrefixes = append(config.OpenAPIConfig.IgnorePrefixes, fmt.Sprintf(
			"/apis/%s/%s/%s/%s", gvr.Group, gvr.Version, gvr.Resource, apiv1.LogSubresourceName,
		))
	}
}

func addDcpHttpHandlers(config *tiltapiserver.Config, ctx context.Context, log logr.Logger) {
	originalChainBuilder := config.GenericConfig.BuildHandlerChainFunc
	config.GenericConfig.BuildHandlerChainFunc = func(handler http.Handler, c *kubeapiserver.Config) http.Handler {
		handler = originalChainBuilder(handler, c)
		handler = withDcpContextValues(handler, ctx, log)
		return handler
	}
}

func runServerFromCompletedConfig(
	config tiltapiserver.CompletedConfig,
	ctx context.Context,
	shutdownRequested func(ApiServerShutdownResourceCleanup),
	log logr.Logger,
) (<-chan struct{}, error) {
	server, err := config.New()
	if err != nil {
		return nil, err
	}

	adminHandler := NewAdminHttpHandler(shutdownRequested, log)
	server.GenericAPIServer.Handler.NonGoRestfulMux.HandlePrefix(AdminPathPrefix, adminHandler)

	server.GenericAPIServer.AddPostStartHookOrDie("start-tilt-server-informers", func(context kubeapiserver.PostStartHookContext) error {
		if config.GenericConfig.SharedInformerFactory != nil {
			// (must use deprecated StopCh because the SharedInformerFactory API requires a stop channel, and not a Context)
			// nolint:staticcheck
			config.GenericConfig.SharedInformerFactory.Start(context.StopCh)
		}
		return nil
	})

	prepared := server.GenericAPIServer.PrepareRun()
	serving := config.ExtraConfig.ServingInfo

	tlsConfig, err := tiltstart.TLSConfig(ctx, serving)
	if err != nil {
		return nil, err
	}

	stopCh := ctx.Done()
	stoppedCh, _, err := kubeapiserver.RunServer(&http.Server{
		Addr:           serving.Listener.Addr().String(),
		Handler:        prepared.Handler,
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      tlsConfig,
	}, serving.Listener, prepared.ShutdownTimeout, stopCh)
	if err != nil {
		return nil, err
	}

	server.GenericAPIServer.RunPostStartHooks(ctx)

	return stoppedCh, nil
}

func RequestApiServerShutdown(ctx context.Context, restClient *clientgorest.RESTClient) error {
	stopRequestData := ApiServerExecutionData{
		Status:                  ApiServerStopping,
		ShutdownResourceCleanup: ApiServerResourceCleanupNone,
	}
	bodyBytes, err := json.Marshal(stopRequestData)
	if err != nil {
		return fmt.Errorf("failed to serialize API server shutdown request: %w", err)
	}

	res := restClient.Patch(types.MergePatchType).
		RequestURI(AdminPathPrefix + ExecutionDocument).
		Body(bodyBytes).
		Do(ctx)
	if res.Error() != nil {
		return fmt.Errorf("API server shutdown request failed: %w", res.Error())
	}
	return nil
}
