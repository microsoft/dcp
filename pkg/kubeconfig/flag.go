package kubeconfig

import (
	goflag "flag"
	"fmt"

	"github.com/spf13/pflag"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// controller-runtime expects --kubeconfig flag to be registered with the default flag.CommandLine flag set,
// see https://github.com/kubernetes-sigs/controller-runtime/blob/master/pkg/client/config/config.go for details.
// Instead of registering the flag ourselves, we will call controller-runtime RegisterFlags() function, then look it up and return.
func EnsureKubeconfigFlag(fs *pflag.FlagSet) *pflag.Flag {
	ensureInCommandLineSet := func() *pflag.Flag {
		ctrl_config.RegisterFlags(goflag.CommandLine) // Idempotent: will register the flag only once in goflag.CommandLine
		return pflag.PFlagFromGoFlag(goflag.CommandLine.Lookup(ctrl_config.KubeconfigFlagName))
	}

	if fs == nil {
		return ensureInCommandLineSet()
	} else {
		f := fs.Lookup(ctrl_config.KubeconfigFlagName)
		if f != nil {
			return f
		}

		f = ensureInCommandLineSet()
		fs.AddFlag(f)
		return f
	}
}

func EnsureKubeconfigFlagValue(flags *pflag.FlagSet) (string, error) {
	f := flags.Lookup(ctrl_config.KubeconfigFlagName)
	if f == nil {
		panic("Unable to find kubeconfig flag. Make sure you call EnsureKubeconfigFlag() before calling this function.")
	}

	kubeconfigPath := f.Value.String()
	if kubeconfigPath != "" {
		return kubeconfigPath, nil // already set
	}

	kubeconfigPath, err := EnsureKubeconfigFile(flags)
	if err != nil {
		return "", fmt.Errorf("unable to ensure existence of a Kubeconfig file: %w", err)
	}

	err = f.Value.Set(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("unable to set kubeconfig flag value: %w", err)
	}

	return kubeconfigPath, nil
}
