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

	"github.com/usvc-dev/stdtypes/pkg/randdata"
)

const DefaultDcpPort = 9562

// Returns path to Kubeconfig file to be used for configuring to the DCP API server,
// and by clients (to connect to the API server).
// If the --kubeconfig parameter is passed, the returned value will be the parameter value.
// If the --kubeconfig parameter is missing, the default location (~/.dcp/kubeconfig) will be checked,
// and if the kubeconfig file is not there, it will be created.
func EnsureKubeconfigFile(fs *pflag.FlagSet) (string, error) {
	f := EnsureKubeconfigFlag(fs)
	if f != nil {
		path := strings.TrimSpace(f.Value.String())

		// If path is empty, this means the user did not pass the --kubeconfig parameter,
		// so fall back to the "check default location" case.
		if path != "" {
			info, err := os.Stat(path)
			if err != nil {
				return "", fmt.Errorf("Could not access Kubeconfig file '%s': %w", path, err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("The Kubeconfig file path '%s' points to a directory, not a file", path)
			}

			return path, nil
		}
	}

	homePath, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("Could not obtain user home directory when checking for Kubeconfig file: %w", err)
	}

	defaultPath := filepath.Join(homePath, ".dcp", "kubeconfig")
	info, err := os.Stat(defaultPath)
	switch {
	case err != nil && !errors.Is(err, iofs.ErrNotExist):
		return "", fmt.Errorf("Could not check whether default Kubeconfig file ('%s') exists: %w", defaultPath, err)
	case err == nil && info.IsDir():
		return "", fmt.Errorf("Kubeconfig file path was not provided and the default location ('%s') is a directory", defaultPath)
	case err == nil:
		return defaultPath, nil
	default:
		if err = createKubeconfigFile(defaultPath); err != nil {
			return "", err
		}
		return defaultPath, nil
	}
}

func createKubeconfigFile(path string) error {
	cluster := clientcmd_api.Cluster{
		Server:                fmt.Sprintf("https://localhost:%d", DefaultDcpPort),
		InsecureSkipTLSVerify: true,
	}

	const authTokenLength = 12
	token, err := randdata.MakeRandomString(authTokenLength)
	if err != nil {
		return fmt.Errorf("Could not generate authentication token for the DCP API server: %w", err)
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

	err = clientcmd.WriteToFile(config, path)
	if err != nil {
		return fmt.Errorf("Could not write Kubeconfig file: %w", err)
	}

	return nil
}
