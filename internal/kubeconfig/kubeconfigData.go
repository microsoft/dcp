package kubeconfig

import (
	"fmt"
	"net/url"
	"strconv"

	clientcmd "k8s.io/client-go/tools/clientcmd"
)

// Reads Kubeconfig data and returns the port and security token of the current context,
// to be used to configure/connect to API server
func GetKubeConfigData(kubeconfigPath string) (int, string, error) {
	const InvalidPort = 0

	kubeConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return InvalidPort, "", fmt.Errorf("could not read Kubeconfig file using path '%s': %w", kubeconfigPath, err)
	}

	kubeContext, found := kubeConfig.Contexts[kubeConfig.CurrentContext]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the context named '%s' (current context) does not exist", kubeConfig.CurrentContext)
	}

	user, found := kubeConfig.AuthInfos[kubeContext.AuthInfo]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}
	if user.Token == "" {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) is missing security token information ('token' property)", kubeContext.AuthInfo)
	}

	cluster, found := kubeConfig.Clusters[kubeContext.Cluster]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}

	clusterUrl, err := url.Parse(cluster.Server)
	if err != nil {
		return InvalidPort, "", fmt.Errorf("could not determine the port to use for the API server; the server URL in Kubeconfig file ('%s') is invalid: %w", cluster.Server, err)
	}

	// If the port is missing, Atoi() will return ErrSyntax
	port, err := strconv.Atoi(clusterUrl.Port())
	if err != nil || port <= InvalidPort {
		return InvalidPort, "", fmt.Errorf("could not determine the port to use for the API server; the port information in server URL ('%s') is either missing or invalid: %w", clusterUrl.Port(), err)
	}

	return port, user.Token, nil
}
