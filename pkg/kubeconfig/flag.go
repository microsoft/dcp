/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package kubeconfig

import (
	"errors"
	goflag "flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	PortFlagName     = "port"
	DCP_SECURE_TOKEN = "DCP_SECURE_TOKEN"
)

var (
	port int32
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

func EnsureKubeconfigPortFlag(fs *pflag.FlagSet) *pflag.Flag {
	if p := fs.Lookup(PortFlagName); p != nil {
		return p
	} else {
		fs.Int32Var(&port, PortFlagName, 0, "Use a specific port when scaffolding the Kubeconfig file. If not specified, a random port will be used.")
		return fs.Lookup(PortFlagName)
	}
}

// Ensures that the kubeconfig flag exist and points to a non-empty file.
// Returns the value of the flag, i.e. path to that file.
func RequireKubeconfigFlagValue(flags *pflag.FlagSet) (string, error) {
	f := flags.Lookup(ctrl_config.KubeconfigFlagName)
	if f == nil {
		return "", fmt.Errorf("unable to find kubeconfig flag. Make sure you call EnsureKubeconfigFlag() before calling this function.")
	}

	if port < 0 || port > 65535 {
		return "", fmt.Errorf("invalid port number: %d", port)
	}

	kubeconfigPath, statErr := getKubeConfigPath(flags)
	if statErr != nil {
		return "", statErr
	}

	info, statErr := os.Stat(kubeconfigPath)
	if statErr != nil && errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("kubeconfig file does not exist at '%s'", kubeconfigPath)
	} else if statErr != nil {
		return "", fmt.Errorf("error retrieving kubeconfig file '%s': %w", kubeconfigPath, statErr)
	} else if info.IsDir() {
		return "", fmt.Errorf("specified kubeconfig ('%s') is a directory", kubeconfigPath)
	} else if info.Size() == 0 {
		return "", fmt.Errorf("kubeconfig file is empty: '%s'", kubeconfigPath)
	}

	flagSetErr := f.Value.Set(kubeconfigPath)
	if flagSetErr != nil {
		return "", fmt.Errorf("unable to set kubeconfig flag value: %w", flagSetErr)
	}

	return kubeconfigPath, nil
}

// Creates API server addressing and authentication data that will go into the kubeconfig file.
// The kubeconfig flag value, if empty upon invocation, will be set to preferred path of the kubeconfig file.
// Does NOT create the kubeconfig file itself (see Kubeconfig.Save() for that).
func EnsureKubeconfigData(flags *pflag.FlagSet, log logr.Logger) (*Kubeconfig, error) {
	f := flags.Lookup(ctrl_config.KubeconfigFlagName)
	if f == nil {
		return nil, fmt.Errorf("unable to find kubeconfig flag. Make sure you call EnsureKubeconfigFlag() before calling this function.")
	}

	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port number: %d", port)
	}

	kubeconfigPath, pathErr := getKubeConfigPath(flags)
	if pathErr != nil {
		return nil, pathErr
	}

	info, statErr := os.Stat(kubeconfigPath)
	if statErr == nil && !info.IsDir() && info.Size() > 0 {
		return nil, fmt.Errorf("kubeconfig file already exists at '%s'", kubeconfigPath)
	}

	// If a token was not provided via DCP_SECURE_TOKEN environment variable, we need to generate one
	token, tokenFound := os.LookupEnv(DCP_SECURE_TOKEN)
	generateToken := !tokenFound || token == ""

	k, kErr := getKubeconfig(kubeconfigPath, port, true /* use certificate */, generateToken, log)
	if kErr != nil {
		return nil, fmt.Errorf("unable to obtain Kubeconfig data: %w", kErr)
	}

	flagSetErr := f.Value.Set(k.Path())
	if flagSetErr != nil {
		return nil, fmt.Errorf("unable to set kubeconfig flag value: %w", flagSetErr)
	}

	return k, nil
}
