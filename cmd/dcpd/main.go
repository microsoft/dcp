package main

import (
	"context"
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/internal/apiserver"
	"github.com/usvc-dev/apiserver/internal/ctrlmanager"
	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/apiserver/internal/logger"
)

type DcpdExitCode int

const (
	OK                  DcpdExitCode = 0
	MemberServiceFailed DcpdExitCode = 1
)

func main() {
	logger, flushLogger := logger.NewLogger()
	ctrlruntime.SetLogger(logger)
	log := runtimelog.Log.WithName("dcpd")

	ctx, cancelFn := context.WithCancel(kubeapiserver.SetupSignalContext())

	apiServer := apiserver.NewApiServer("api-server", flushLogger)
	hostingSvc := []hosting.Service{
		apiServer,
		ctrlmanager.NewManager(apiServer.PortInfo, flushLogger, "controller-manager"),
	}

	var exitCode DcpdExitCode = OK
	host := &hosting.Host{
		Services: hostingSvc,
		Logger:   log,
	}
	stopped, serviceErrors := host.RunAsync(ctx)

	select {
	case <-ctx.Done():
		// Shutdown triggered
		log.Info("shutting down")
	case svcErr := <-serviceErrors:
		log.Error(svcErr.Err, fmt.Sprintf("service %s exited with an error", svcErr.Name))
		exitCode = MemberServiceFailed
	}
	cancelFn()

	// Finished shutting down. An error returned here is a failure to terminate gracefully,
	// so just crash if that happens.
	err := <-stopped
	if err == nil {
		os.Exit(int(exitCode))
	} else {
		panic(err)
	}
}
