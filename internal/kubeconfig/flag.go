package kubeconfig

import (
	"flag"

	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// controller-runtime expects --kubeconfig flag to be registered with the default flag.CommandLine flag set,
// see https://github.com/kubernetes-sigs/controller-runtime/blob/master/pkg/client/config/config.go for details.
// Instead of registering the flag ourselves, we will call controller-runtime RegisterFlags() function, then look it up and return.
func GetKubeconfigFlag() *flag.Flag {
	ctrl_config.RegisterFlags(flag.CommandLine)
	f := flag.CommandLine.Lookup(ctrl_config.KubeconfigFlagName)
	return f
}
