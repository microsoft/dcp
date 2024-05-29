package exerunners

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// A set of data related to how IDE requests are formed.
type ideConnectionInfo struct {
	portStr         string // The local port on which the IDE is listening for run session requests
	tokenStr        string // The security token to use when connecting to the IDE
	httpScheme      string // The scheme to use when connecting to the IDE (HTTP or HTTPS)
	webSocketScheme string // The scheme to use when connecting to the IDE via websockets (ws or wss)
	instanceId      string // The DCP instance ID to use when connecting to the IDE

	// The value of the api-version parameter, indicating protocol version the runner is using when talking tot the IDE
	// If empty, it indicates "pre-Aspire GA" (obsolete) protocol version.
	// TODO: remove pre-Aspire GA support after Aspire GA is released.
	apiVersion apiVersion

	httpClient *http.Client      // The client to make HTTP requests to the IDE
	wsDialer   *websocket.Dialer // The dialer to use when connecting to the IDE via WebSocket protocol
}

func NewIdeConnectionInfo(lifetimeCtx context.Context, log logr.Logger) (*ideConnectionInfo, error) {
	const runnerNotAvailable = "Executables cannot be started via IDE: "
	const missingRequiredEnvVar = "missing required environment variable '%s'"

	createAndLogError := func(format string, a ...any) error {
		err := fmt.Errorf(format, a...)
		log.Info(runnerNotAvailable + err.Error())
		return err
	}

	portStr, found := os.LookupEnv(ideEndpointPortVar)
	if !found {
		return nil, createAndLogError(missingRequiredEnvVar, ideEndpointPortVar)
	}
	portStr = strings.TrimSpace(portStr)
	portStr = strings.TrimPrefix(portStr, "localhost:") // Visual Studio prepends "localhost:" to port number.

	tokenStr, found := os.LookupEnv(ideEndpointTokenVar)
	if !found {
		return nil, createAndLogError(missingRequiredEnvVar, ideEndpointTokenVar)
	}
	tokenStr = strings.TrimSpace(tokenStr)

	client := http.Client{}
	wsDialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: defaultIdeEndpointRequestTimeout,
	}
	httpScheme := "http"
	webSocketScheme := "ws"

	serverCertEncodedBytes, certFound := os.LookupEnv(ideEndpointCertVar)
	if certFound {
		certBytes, decodeErr := base64.StdEncoding.AppendDecode(nil, []byte(serverCertEncodedBytes))
		if decodeErr != nil {
			return nil, createAndLogError("failed to decode the server certificate: %w Secure communication with the IDE is not possible", decodeErr)
		}

		cert, certParseErr := x509.ParseCertificate(certBytes)
		if certParseErr != nil {
			return nil, createAndLogError("failed to decode the server certificate: %w Secure communication with the IDE is not possible", certParseErr)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AddCert(cert)
		tlsConfig := &tls.Config{
			RootCAs: caCertPool,
		}
		client.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		wsDialer.TLSClientConfig = tlsConfig
		httpScheme = "https"
		webSocketScheme = "wss"
	}

	connInfo := ideConnectionInfo{
		portStr:         portStr,
		tokenStr:        tokenStr,
		httpScheme:      httpScheme,
		webSocketScheme: webSocketScheme,
		httpClient:      &client,
		wsDialer:        &wsDialer,
		instanceId:      networking.GetProgramInstanceID(),
	}

	// Query for supported protocol version
	req, reqCancel, reqCreationErr := connInfo.MakeIdeRequest(
		lifetimeCtx,
		ideRunSessionInfoPath,
		http.MethodGet,
		nil, // No body for this request
	)
	if reqCreationErr != nil {
		return nil, createAndLogError("failed to create IDE endpoint info request: %w", reqCreationErr)
	}
	defer reqCancel()

	clientResp, reqErr := client.Do(req)

	// We fall back to pre-Aspire GA protocol version if the request fails.
	if reqErr == nil && clientResp.StatusCode == http.StatusOK {
		defer clientResp.Body.Close()
		respBody, bodyReadErr := io.ReadAll(clientResp.Body)
		if bodyReadErr == nil {
			var info infoResponse
			unmarshalErr := json.Unmarshal(respBody, &info)
			if unmarshalErr != nil {
				log.Error(unmarshalErr, "failed to parse IDE info response")
			} else if slices.Contains(info.ProtocolsSupported, version20240423) {
				connInfo.apiVersion = version20240423
			} else if slices.Contains(info.ProtocolsSupported, version20240303) {
				connInfo.apiVersion = version20240303
			}
		}
	}

	log.V(1).Info("IDE connection info created",
		"port", portStr,
		"httpScheme", httpScheme,
		"webSocketScheme", webSocketScheme,
		"instanceId", networking.GetProgramInstanceID(),
		"apiVersion", connInfo.apiVersion,
	)

	return &connInfo, nil
}

func (connInfo *ideConnectionInfo) MakeIdeRequest(
	parentCtx context.Context,
	requestPath string,
	httpMethod string,
	body io.Reader,
) (*http.Request, context.CancelFunc, error) {
	var url string
	if connInfo.apiVersion != "" {
		url = fmt.Sprintf("%s://localhost:%s%s?%s=%s", connInfo.httpScheme, connInfo.portStr, requestPath, queryParamApiVersion, connInfo.apiVersion)
	} else {
		url = fmt.Sprintf("%s://localhost:%s%s", connInfo.httpScheme, connInfo.portStr, requestPath)
	}
	reqCtx, reqCtxCancel := context.WithTimeout(parentCtx, defaultIdeEndpointRequestTimeout)

	req, reqCreationErr := http.NewRequestWithContext(reqCtx, httpMethod, url, body)
	if reqCreationErr != nil {
		reqCtxCancel()
		return nil, nil, fmt.Errorf("failed to create IDE endpoint info request: %w", reqCreationErr)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", connInfo.tokenStr))
	req.Header.Set(instanceIdHeader, connInfo.instanceId)

	// Assume the body is JSON if it is provided
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, reqCtxCancel, nil
}

func (connInfo *ideConnectionInfo) GetClient() *http.Client {
	return connInfo.httpClient
}

func (connInfo *ideConnectionInfo) GetDialer() *websocket.Dialer {
	return connInfo.wsDialer
}
