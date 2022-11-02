package ctrlmanager

import (
	"context"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	scheme = apiruntime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func RunManager(port int, flushLogger func(), ctx context.Context) error {
	// Assumes log.SetLogger() was called by the caller
	log := runtimelog.Log.WithName("controller-manager")
	defer flushLogger()

	mgr, err := ctrlruntime.NewManager(ctrlruntime.GetConfigOrDie(), ctrlruntime.Options{
		Scheme:         scheme,
		Port:           port,
		LeaderElection: false,
	})
	if err != nil {
		log.Error(err, "unable to create new  contoller manager")
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
