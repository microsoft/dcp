package kubeconfig

import (
	goflag "flag"

	"github.com/spf13/pflag"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// controller-runtime expects --kubeconfig flag to be registered with the default flag.CommandLine flag set,
// see https://github.com/kubernetes-sigs/controller-runtime/blob/master/pkg/client/config/config.go for details.
// Instead of registering the flag ourselves, we will call controller-runtime RegisterFlags() function, then look it up and return.
func GetKubeconfigFlag(fs *pflag.FlagSet) *pflag.Flag {
	if fs == nil {
		ctrl_config.RegisterFlags(goflag.CommandLine) // Idempotent: will register the flag only once in goflag.CommandLine
		return pflag.PFlagFromGoFlag(goflag.CommandLine.Lookup(ctrl_config.KubeconfigFlagName))
	} else {
		f := fs.Lookup(ctrl_config.KubeconfigFlagName)
		if f != nil {
			return f
		}
		gfs := goflag.NewFlagSet("tmp", goflag.ContinueOnError)
		ctrl_config.RegisterFlags(gfs)
		fs.AddGoFlagSet(gfs)
		return fs.Lookup(ctrl_config.KubeconfigFlagName)
	}
}
