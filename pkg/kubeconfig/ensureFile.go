package kubeconfig

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmd_api "k8s.io/client-go/tools/clientcmd/api"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

func doEnsureKubeconfigFile(configPath string, port int32) (string, error) {
	info, err := os.Stat(configPath)
	if err != nil && errors.Is(err, iofs.ErrNotExist) {
		if err = createKubeconfigFile(configPath, port); err != nil {
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

	homePath, homeDirErr := os.UserHomeDir()
	if homeDirErr != nil {
		return "", fmt.Errorf("could not obtain user home directory when checking for Kubeconfig file: %w", homeDirErr)
	}

	dcpFolder := filepath.Join(homePath, ".dcp")
	dcpFolderInfo, dcpFolderErr := os.Stat(dcpFolder)
	if errors.Is(dcpFolderErr, iofs.ErrNotExist) {
		if err := os.MkdirAll(dcpFolder, osutil.PermissionOnlyOwnerReadWriteSetCurrent); err != nil {
			return "", fmt.Errorf("failed to create DCP default folder '%s' for storing kubeconfig file: %w", dcpFolder, err)
		}
	} else if dcpFolderErr != nil {
		return "", fmt.Errorf("failed to verify the existence of DCP  default folder '%s': %w", dcpFolder, dcpFolderErr)
	} else if !dcpFolderInfo.IsDir() {
		return "", fmt.Errorf("'%s' exists, but is not a directory and cannot be used to store DCP kubeconfig file", dcpFolder)
	}

	kubeconfigPath := filepath.Join(dcpFolder, "kubeconfig")
	return doEnsureKubeconfigFile(kubeconfigPath, port)
}

func createKubeconfigFile(path string, port int32) error {
	// The "localhost" hostname resolves to all loopback addresses on the machine. For dual-stack machines (very common)
	// it will contain both IPv4 and IPv6 addresses. However, different programming languages and libraries may
	// "choose" different addresses to try first (e.g. some might prefer IPv4 vs IPv6).
	// The result can be long connection delays. To avoid that we will use specific IP address.
	ips, err := net.LookupIP("localhost")
	if err != nil {
		return fmt.Errorf("kubeconfig file creation failed: could not obtain IP address(es) for localhost: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("kubeconfig file creation failed: could not obtain IP address(es) for localhost")
	}

	ip := ips[0]
	address := networking.IpToString(ip)

	if port == 0 {
		if newPort, newPortErr := networking.GetFreePort(apiv1.TCP, address); newPortErr != nil {
			return newPortErr
		} else {
			port = newPort
		}
	}

	cluster := clientcmd_api.Cluster{
		Server:                fmt.Sprintf("https://%s:%d", address, port),
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

	if err = usvc_io.WriteFile(path, contents, osutil.PermissionOnlyOwnerReadWrite); err != nil {
		return fmt.Errorf("could not write Kubeconfig file: %w", err)
	}

	return nil
}
