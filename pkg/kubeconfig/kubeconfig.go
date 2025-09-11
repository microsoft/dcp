package kubeconfig

import (
	"bytes"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	iofs "io/fs"
	"math/big"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmd_api "k8s.io/client-go/tools/clientcmd/api"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

const (
	caKeyLength           = 4096
	keyLength             = 2048
	defaultExpirationDays = 7
	PlaceholderToken      = "<placeholder>"
)

type certificateData struct {
	caCertificate     []byte
	serverCertificate []byte
	serverKey         *rsa.PrivateKey
}

func (c *certificateData) CA() ([]byte, error) {
	caBuffer := bytes.Buffer{}
	if err := pem.Encode(&caBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: c.caCertificate}); err != nil {
		return nil, err
	}

	return caBuffer.Bytes(), nil
}

func (c *certificateData) Certificate() ([]byte, error) {
	certBuffer := bytes.Buffer{}
	if err := pem.Encode(&certBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: c.serverCertificate}); err != nil {
		return nil, err
	}
	if err := pem.Encode(&certBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: c.caCertificate}); err != nil {
		return nil, err
	}

	return certBuffer.Bytes(), nil
}

func (c *certificateData) Key() ([]byte, error) {
	keyBuffer := bytes.Buffer{}
	if err := pem.Encode(&keyBuffer, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(c.serverKey)}); err != nil {
		return nil, err
	}

	return keyBuffer.Bytes(), nil
}

type Kubeconfig struct {
	Config          *clientcmd_api.Config
	certificateData *certificateData
	path            string
}

func (k *Kubeconfig) Path() string {
	return k.path
}

// Write the kubeconfig data if it doesn't exist. Will not update an existing kubeconfig.
func (k *Kubeconfig) Save() error {
	info, err := os.Stat(k.path)

	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return err
	}

	if err == nil && info.IsDir() {
		return fmt.Errorf("specified kubeconfig ('%s') is a directory", k.path)
	}

	if err == nil && info.Size() > 0 {
		return fmt.Errorf("kubeconfig file already exists at '%s'", k.path)
	}

	contents, contentErr := clientcmd.Write(*k.Config)
	if contentErr != nil {
		return fmt.Errorf("could not write Kubeconfig file: %w", contentErr)
	}

	// Write file to a temporary file first, then rename the file to the final name.
	// This avoids failures related to file locking and clients reading partially-written file.
	suffix, suffixErr := randdata.MakeRandomString(6)
	if suffixErr != nil {
		return fmt.Errorf("could not generate random suffix for new kubeconfig file: %w", suffixErr)
	}

	if writeErr := usvc_io.WriteFile(k.path+string(suffix), contents, osutil.PermissionOnlyOwnerReadWrite); writeErr != nil {
		return fmt.Errorf("could not write Kubeconfig file: %w", writeErr)
	}

	if renameErr := os.Rename(k.path+string(suffix), k.path); renameErr != nil {
		return fmt.Errorf("could not rename temporary Kubeconfig file to final location: %w", renameErr)
	}

	return nil
}

// Reads Kubeconfig data and returns the address, port and security token of the current context,
// to be used to configure/connect to API server
func (k *Kubeconfig) GetData() (net.IP, int, string, *certificateData, error) {
	kubeContext, found := k.Config.Contexts[k.Config.CurrentContext]
	if !found {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("kubeconfig file is invalid; the context named '%s' (current context) does not exist", k.Config.CurrentContext)
	}

	user, found := k.Config.AuthInfos[kubeContext.AuthInfo]
	if !found {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}

	cluster, found := k.Config.Clusters[kubeContext.Cluster]
	if !found {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}

	clusterUrl, err := url.Parse(cluster.Server)
	if err != nil {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("could not determine the port to use for the API server; the server URL in Kubeconfig file ('%s') is invalid: %w", cluster.Server, err)
	}

	// If the port is missing, Atoi() will return ErrSyntax
	port, err := strconv.Atoi(clusterUrl.Port())
	if err != nil || port <= networking.InvalidPort {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("could not determine the port to use for the API server; the port information in server URL ('%s') is either missing or invalid: %w", clusterUrl.Port(), err)
	}

	ips, err := networking.LookupIP(clusterUrl.Hostname())
	if err != nil || len(ips) == 0 {
		return nil, networking.InvalidPort, "", nil, fmt.Errorf("could not determine the network address to use for the API server; the host name information in server URL ('%s') is either missing or invalid: %w", clusterUrl.Hostname(), err)
	}

	// We are going to take the first resolved address, because Kubernetes API server does not support
	// binding to multiple addresses and we have no extra information to prefer one over another.
	return ips[0], port, user.Token, k.certificateData, nil
}

// Get the path to the kubeconfig from the flag data.
func getKubeConfigPath(fs *pflag.FlagSet) (string, error) {
	f := EnsureKubeconfigFlag(fs)
	if f != nil {
		path := strings.TrimSpace(f.Value.String())

		// If path is empty, this means the user did not pass the --kubeconfig parameter,
		// so fall back to the "check default location" case.
		if path != "" {
			return path, nil
		}
	}

	var kubeconfigFolder string
	if usvc_io.DcpSessionDir() != "" {
		kubeconfigFolder = usvc_io.DcpSessionDir()
	} else {
		var dcpFolderErr error
		kubeconfigFolder, dcpFolderErr = dcppaths.EnsureUserDcpDir()
		if dcpFolderErr != nil {
			return "", fmt.Errorf("could not determine the path to the DCP default folder; do not know where to find or create the kubeconfig file: %w", dcpFolderErr)
		}
	}

	kubeconfigPath := filepath.Join(kubeconfigFolder, "kubeconfig")
	return kubeconfigPath, nil
}

// For an existing kubeconfig file, read the data and return it. If no kubeconfig file exists, generate the
// data and return that (to be persisted after API server starts).
func getKubeconfig(kubeconfigPath string, port int32, useCertificate bool, generateToken bool, log logr.Logger) (*Kubeconfig, error) {
	info, err := os.Stat(kubeconfigPath)

	var config *clientcmd_api.Config
	var certificateData *certificateData
	if err != nil {
		if !errors.Is(err, iofs.ErrNotExist) {
			return nil, fmt.Errorf("error retrieving kubeconfig file '%s': %w", kubeconfigPath, err)
		}

		// Create a new config
		config, certificateData, err = createKubeconfig(port, useCertificate, generateToken, log)
		if err != nil {
			return nil, err
		}
	} else if info.IsDir() {
		return nil, fmt.Errorf("specified kubeconfig ('%s') is a directory", kubeconfigPath)
	} else {
		// Read the existing config
		config, err = clientcmd.LoadFromFile(kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("could not read Kubeconfig file '%s': %w", kubeconfigPath, err)
		}
	}

	return &Kubeconfig{
		Config:          config,
		path:            kubeconfigPath,
		certificateData: certificateData,
	}, nil
}

func generateCertificates(ip net.IP) (*certificateData, error) {
	// Generate keys for the CA certificate; do not persist after creating the certificates
	caKey, caKeyErr := rsa.GenerateKey(cryptorand.Reader, caKeyLength)
	if caKeyErr != nil {
		return nil, caKeyErr
	}

	// Generate keys for the server certificate
	serverKey, serverKeyErr := rsa.GenerateKey(cryptorand.Reader, keyLength)
	if serverKeyErr != nil {
		return nil, serverKeyErr
	}

	// Template for the CA certificate
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(rand.Int63()),
		Subject: pkix.Name{
			CommonName: ip.String(),
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(0, 0, defaultExpirationDays),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, caErr := x509.CreateCertificate(cryptorand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if caErr != nil {
		return nil, caErr
	}

	// Generate the subject ID for the server certificate as a SHA256 hash of the server public key
	serverPublicKeyBytes, serverPublicKeyBytesErr := asn1.Marshal(*serverKey.Public().(*rsa.PublicKey))
	if serverPublicKeyBytesErr != nil {
		return nil, serverPublicKeyBytesErr
	}
	serverPublicKeySubjectId := sha256.Sum256(serverPublicKeyBytes)

	// Template for the server certificate
	server := &x509.Certificate{
		SerialNumber: big.NewInt(rand.Int63()),
		Subject:      pkix.Name{},
		IPAddresses:  []net.IP{ip},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(0, 0, defaultExpirationDays),
		SubjectKeyId: serverPublicKeySubjectId[:],
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	serverBytes, serverErr := x509.CreateCertificate(cryptorand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if serverErr != nil {
		return nil, serverErr
	}

	return &certificateData{
		caCertificate:     caBytes,
		serverCertificate: serverBytes,
		serverKey:         serverKey,
	}, nil
}

func createKubeconfig(port int32, useCertifiate bool, generateToken bool, log logr.Logger) (*clientcmd_api.Config, *certificateData, error) {
	ips, err := networking.GetPreferredHostIps(networking.Localhost)
	if err != nil {
		return nil, nil, fmt.Errorf("kubeconfig file creation failed: %w", err)
	}

	ip := ips[0]
	address := networking.IpToString(ip)

	if port == 0 {
		if newPort, newPortErr := networking.GetFreePort(apiv1.TCP, address, log); newPortErr != nil {
			return nil, nil, newPortErr
		} else {
			port = newPort
		}
	}

	cluster := clientcmd_api.Cluster{
		Server: "https://" + networking.AddressAndPort(address, port),
	}

	var certificateData *certificateData
	var certificateErr error
	if useCertifiate {
		// Generate certificates to secure the connection
		certificateData, certificateErr = generateCertificates(ip)
		if certificateErr != nil {
			return nil, nil, fmt.Errorf("kubeconfig file creation failed: could not generate certificates: %w", certificateErr)
		}

		caPEM := new(bytes.Buffer)
		// PEM encode the CA certificate
		certificateErr = pem.Encode(caPEM, &pem.Block{Type: "CERTIFICATE", Bytes: certificateData.caCertificate})
		if certificateErr != nil {
			return nil, nil, fmt.Errorf("kubeconfig file creation failed: could not encode certificates: %w", certificateErr)
		}

		// We're generating a certificate, so we need to tell the client how to verify it
		cluster.CertificateAuthorityData = caPEM.Bytes()
	} else {
		// If we aren't generating certificates, we need to skip TLS verification
		cluster.InsecureSkipTLSVerify = true
	}

	user := clientcmd_api.AuthInfo{}
	if generateToken {
		const authTokenLength = 32
		token, tokenErr := randdata.MakeRandomString(authTokenLength)
		if tokenErr != nil {
			return nil, nil, fmt.Errorf("could not generate authentication token for the DCP API server: %w", tokenErr)
		}
		user.Token = string(token)
	} else {
		user.Token = PlaceholderToken
	}

	context := clientcmd_api.Context{
		Cluster:  "apiserver_cluster",
		AuthInfo: "apiserver_user",
	}

	return &clientcmd_api.Config{
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
	}, certificateData, nil
}
