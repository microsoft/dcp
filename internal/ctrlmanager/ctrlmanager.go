package ctrlmanager

import (
	"context"
	"fmt"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	msgManagerCreationFailed = "unable to create controller manager"
)

var (
	scheme = apiruntime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

type CtrlManager struct {
	portSource  <-chan int
	flushLogger func()
	name        string
}

func NewManager(portSource <-chan int, flushLogger func(), name string) *CtrlManager {
	return &CtrlManager{portSource: portSource, flushLogger: flushLogger, name: name}
}

func (m *CtrlManager) Name() string {
	return m.name
}

func (m *CtrlManager) Run(ctx context.Context) error {
	// Assumes log.SetLogger() was already called
	log := runtimelog.Log.WithName(m.name)
	defer m.flushLogger()

	var port int
	select {
	case port = <-m.portSource:
		break
	case <-ctx.Done():
		err := fmt.Errorf("Did not receive port information for connecting to API server before request to shut down: %w", ctx.Err())
		log.Error(err, msgManagerCreationFailed)
		return err
	}
	if port == 0 {
		err := fmt.Errorf("Did not receive port information for connecting to API server")
		log.Error(err, msgManagerCreationFailed)
		return err
	}

	mgr, err := ctrlruntime.NewManager(ctrlruntime.GetConfigOrDie(), ctrlruntime.Options{
		Scheme:         scheme,
		Port:           port,
		LeaderElection: false,
	})
	if err != nil {
		log.Error(err, msgManagerCreationFailed)
		return err
	}

	log.Info("starting controller manager")
	err = mgr.Start(ctx)
	if err != nil {
		log.Error(err, "contoller manager failed")
		return err
	}

	return nil
}
