/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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
	netutil "k8s.io/apimachinery/pkg/util/net"

	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/slices"
)

// A set of data related to how IDE requests are formed.
type ideConnectionInfo struct {
	portStr         string // The local port on which the IDE is listening for run session requests
	tokenStr        string // The security token to use when connecting to the IDE
	httpScheme      string // The scheme to use when connecting to the IDE (HTTP or HTTPS)
	webSocketScheme string // The scheme to use when connecting to the IDE via websockets (ws or wss)
	instanceId      string // The DCP instance ID to use when connecting to the IDE

	// The API version we will use to communicate with the IDE (the highest version that both sides support)
	apiVersion apiVersion

	supportedApiVersions          []apiVersion // The list of API versions supported by the IDE
	supportedLaunchConfigurations []string     // The list of launch configurations supported by the IDE

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
		Proxy:            netutil.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment),
		HandshakeTimeout: ideEndpointRequestTimeout,
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
	if clientResp != nil && clientResp.Body != nil {
		defer clientResp.Body.Close()
	}

	if reqErr == nil && clientResp.StatusCode == http.StatusOK {
		respBody, bodyReadErr := io.ReadAll(clientResp.Body)
		if bodyReadErr == nil {
			var info infoResponse
			unmarshalErr := json.Unmarshal(respBody, &info)
			if unmarshalErr != nil {
				log.Error(unmarshalErr, "Failed to parse IDE info response")
			} else {
				connInfo.supportedApiVersions = info.ProtocolsSupported

				// We will use the IDE endpoint ONLY IF we support at least one common API version
				if slices.Contains(info.ProtocolsSupported, version20251001) {
					connInfo.apiVersion = version20251001
				} else if slices.Contains(info.ProtocolsSupported, version20240423) {
					connInfo.apiVersion = version20240423
				} else if slices.Contains(info.ProtocolsSupported, version20240303) {
					connInfo.apiVersion = version20240303
				}

				if len(info.SupportedLaunchConfigurationTypes) > 0 {
					connInfo.supportedLaunchConfigurations = info.SupportedLaunchConfigurationTypes
				} else {
					// For backward compatibility, assume that if the IDE does not report supported launch configurations,
					// it supports "project" configuration.
					connInfo.supportedLaunchConfigurations = []string{vsProjectLaunchConfiguration}
				}
			}
		}
	}

	if connInfo.apiVersion == "" {
		return nil, createAndLogError("an old or incompatible Aspire IDE extension was detected; only Aspire 8.0 and later versions are supported")
	}

	log.V(1).Info("IDE connection info created",
		"Port", portStr,
		"HttpScheme", httpScheme,
		"WebSocketScheme", webSocketScheme,
		"InstanceId", networking.GetProgramInstanceID(),
		"ApiVersion", connInfo.apiVersion,
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
	reqCtx, reqCtxCancel := context.WithTimeout(parentCtx, ideEndpointRequestTimeout)

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
