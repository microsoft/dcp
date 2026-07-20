/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package kubeconfig

import (
	"context"
	"errors"
	goflag "flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/security"
)

const (
	PortFlagName              = "port"
	TLSCertFileFlagName       = "tls-cert-file"
	TLSKeyFileFlagName        = "tls-key-file"
	TLSCertThumbprintFlagName = "tls-cert-thumbprint"
	TLSCAFileFlagName         = "tls-ca-file"
	DCP_SECURE_TOKEN          = "DCP_SECURE_TOKEN"
)

var (
	port              int32
	tlsCertFile       string
	tlsKeyFile        string
	tlsCertThumbprint string
	tlsCAFile         string
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

// EnsureTLSCertFileFlag registers the --tls-cert-file flag if not already registered.
func EnsureTLSCertFileFlag(fs *pflag.FlagSet) *pflag.Flag {
	if f := fs.Lookup(TLSCertFileFlagName); f != nil {
		return f
	}
	fs.StringVar(&tlsCertFile, TLSCertFileFlagName, "",
		"File containing the PEM-encoded server certificate chain to use for HTTPS. "+
			"Must be specified with --"+TLSKeyFileFlagName+". "+
			"If not provided, DCP uses --"+TLSCertThumbprintFlagName+" or generates an ephemeral self-signed certificate.")
	return fs.Lookup(TLSCertFileFlagName)
}

// EnsureTLSKeyFileFlag registers the --tls-key-file flag if not already registered.
func EnsureTLSKeyFileFlag(fs *pflag.FlagSet) *pflag.Flag {
	if f := fs.Lookup(TLSKeyFileFlagName); f != nil {
		return f
	}
	fs.StringVar(&tlsKeyFile, TLSKeyFileFlagName, "",
		"File containing the PEM-encoded private key for --"+TLSCertFileFlagName+".")
	return fs.Lookup(TLSKeyFileFlagName)
}

// EnsureTLSCertThumbprintFlag registers the --tls-cert-thumbprint flag if not already registered.
func EnsureTLSCertThumbprintFlag(fs *pflag.FlagSet) *pflag.Flag {
	if f := fs.Lookup(TLSCertThumbprintFlagName); f != nil {
		return f
	}
	fs.StringVar(&tlsCertThumbprint, TLSCertThumbprintFlagName, "",
		"SHA-1 thumbprint of the certificate to use for HTTPS. "+
			"When used with --"+TLSCertFileFlagName+", verifies the loaded certificate. "+
			"When used without certificate files, looks up a certificate in the Windows CurrentUser\\My certificate store.")
	return fs.Lookup(TLSCertThumbprintFlagName)
}

// EnsureTLSCAFileFlag registers the --tls-ca-file flag if not already registered.
func EnsureTLSCAFileFlag(fs *pflag.FlagSet) *pflag.Flag {
	if f := fs.Lookup(TLSCAFileFlagName); f != nil {
		return f
	}
	fs.StringVar(&tlsCAFile, TLSCAFileFlagName, "",
		"File containing the PEM-encoded CA certificate to use as the trust anchor. "+
			"Used with --"+TLSCertFileFlagName+" or --"+TLSCertThumbprintFlagName+" when the certificate is not self-signed.")
	return fs.Lookup(TLSCAFileFlagName)
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
//
// ctx is used to propagate trace context for sub-instrumentation and cancellation for port allocation.
func EnsureKubeconfigData(ctx context.Context, flags *pflag.FlagSet, log logr.Logger) (*Kubeconfig, error) {
	f := flags.Lookup(ctrl_config.KubeconfigFlagName)
	if f == nil {
		return nil, fmt.Errorf("unable to find kubeconfig flag. Make sure you call EnsureKubeconfigFlag() before calling this function.")
	}

	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port number: %d", port)
	}

	// Determine the server address and optional store certificate data.
	var serverAddress string
	var storeCertData *security.ServerCertificateData
	hasTLSCertFile := tlsCertFile != ""
	hasTLSKeyFile := tlsKeyFile != ""
	if hasTLSCertFile != hasTLSKeyFile {
		return nil, fmt.Errorf("--%s and --%s must be specified together", TLSCertFileFlagName, TLSKeyFileFlagName)
	}

	if hasTLSCertFile {
		lookup, lookupErr := traced(ctx, spanTlsCertLookup, func() (certLookup, error) {
			cd, addr, err := security.LoadCertificateFiles(tlsCertFile, tlsKeyFile, tlsCAFile, tlsCertThumbprint)
			return certLookup{data: cd, address: addr}, err
		})
		if lookupErr != nil {
			return nil, fmt.Errorf("TLS certificate file load failed: %w", lookupErr)
		}
		storeCertData, serverAddress = lookup.data, lookup.address
	} else if tlsCertThumbprint != "" {
		lookup, lookupErr := traced(ctx, spanTlsCertLookup, func() (certLookup, error) {
			cd, addr, err := security.LookupCertificate(tlsCertThumbprint)
			return certLookup{data: cd, address: addr}, err
		})
		if lookupErr != nil {
			return nil, fmt.Errorf("TLS certificate store lookup failed: %w", lookupErr)
		}
		storeCertData, serverAddress = lookup.data, lookup.address

		// If a CA file is provided, use it as the trust anchor instead of the cert itself.
		if tlsCAFile != "" {
			caPEM, caReadErr := security.LoadCertificateAuthorityFile(tlsCAFile)
			if caReadErr != nil {
				return nil, caReadErr
			}
			storeCertData.CACertPEM = caPEM
		}
	} else {
		if tlsCAFile != "" {
			return nil, fmt.Errorf("--%s requires --%s/--%s or --%s to also be specified", TLSCAFileFlagName, TLSCertFileFlagName, TLSKeyFileFlagName, TLSCertThumbprintFlagName)
		}
		preferredAddress, preferredErr := traced(ctx, spanPreferredHostIps, func() (string, error) {
			ips, err := networking.GetPreferredHostIps(networking.Localhost)
			if err != nil {
				return "", err
			}
			return networking.IpToString(ips[0]), nil
		})
		if preferredErr != nil {
			return nil, fmt.Errorf("could not determine server address: %w", preferredErr)
		}
		serverAddress = preferredAddress
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

	generateEphemeral := storeCertData == nil

	k, kErr := getKubeconfig(ctx, kubeconfigPath, port, generateEphemeral, generateToken, storeCertData, serverAddress, log)
	if kErr != nil {
		return nil, fmt.Errorf("unable to obtain Kubeconfig data: %w", kErr)
	}

	flagSetErr := f.Value.Set(k.Path())
	if flagSetErr != nil {
		return nil, fmt.Errorf("unable to set kubeconfig flag value: %w", flagSetErr)
	}

	return k, nil
}

// certLookup bundles the two return values of security.LookupCertificate so the
// surrounding traced helper can carry a single generic result type.
type certLookup struct {
	data    *security.ServerCertificateData
	address string
}
