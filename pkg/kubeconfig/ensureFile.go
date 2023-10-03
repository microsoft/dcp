package kubeconfig

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmd_api "k8s.io/client-go/tools/clientcmd/api"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

func doEnsureKubeconfigFile(configPath string, port int32) (string, error) {
	info, err := os.Stat(configPath)
	if err != nil && errors.Is(err, iofs.ErrNotExist) {
		if err := createKubeconfigFile(configPath, port); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", fmt.Errorf("could not check whether kubeconfig file ('%s') exists: %w", configPath, err)
	} else if info.IsDir() {
		return "", fmt.Errorf("specified kubeconfig ('%s') is a directory", configPath)
	}

	return configPath, nil
}

// Returns path to Kubeconfig file to be used for configuring to the DCP API server,
// and by clients (to connect to the API server).
// If the --kubeconfig parameter is passed, the returned value will be the parameter value.
// If the --kubeconfig parameter is missing, the default location (~/.dcp/kubeconfig) will be checked,
// and if the kubeconfig file is not there, it will be created.
func EnsureKubeconfigFile(fs *pflag.FlagSet, port int32) (string, error) {
	f := EnsureKubeconfigFlag(fs)
	if f != nil {
		path := strings.TrimSpace(f.Value.String())

		// If path is empty, this means the user did not pass the --kubeconfig parameter,
		// so fall back to the "check default location" case.
		if path != "" {
			return doEnsureKubeconfigFile(path, port)
		}
	}

	homePath, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not obtain user home directory when checking for Kubeconfig file: %w", err)
	}

	defaultPath := filepath.Join(homePath, ".dcp", "kubeconfig")
	return doEnsureKubeconfigFile(defaultPath, port)
}

func createKubeconfigFile(path string, port int32) error {
	if port == 0 {
		if newPort, err := networking.GetFreePort(apiv1.TCP, "localhost"); err != nil {
			return err
		} else {
			port = newPort
		}
	}

	cluster := clientcmd_api.Cluster{
		Server:                fmt.Sprintf("https://localhost:%d", port),
		InsecureSkipTLSVerify: true,
	}

	const authTokenLength = 12
	token, err := randdata.MakeRandomString(authTokenLength)
	if err != nil {
		return fmt.Errorf("could not generate authentication token for the DCP API server: %w", err)
	}
	user := clientcmd_api.AuthInfo{
		Token: string(token),
	}

	context := clientcmd_api.Context{
		Cluster:  "apiserver_cluster",
		AuthInfo: "apiserver_user",
	}

	config := clientcmd_api.Config{
		Kind:       "Config",
		APIVersion: "v1",
		Clusters: map[string]*clientcmd_api.Cluster{
			"apiserver_cluster": &cluster,
		},
		AuthInfos: map[string]*clientcmd_api.AuthInfo{
			"apiserver_user": &user,
		},
		Contexts: map[string]*clientcmd_api.Context{
			"apiserver": &context,
		},
		CurrentContext: "apiserver",
	}

	contents, err := clientcmd.Write(config)
	if err != nil {
		return fmt.Errorf("could not write Kubeconfig file: %w", err)
	}

	if err := io.WriteFile(path, contents, osutil.PermissionOnlyOwnerReadWrite); err != nil {
		return fmt.Errorf("could not write Kubeconfig file: %w", err)
	}

	return nil
}
