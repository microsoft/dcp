/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryContentServesImage(t *testing.T) {
	t.Parallel()

	content, contentErr := newRegistryContent("dcp/test-image", map[string]string{"owner": "test"})
	require.NoError(t, contentErr)

	testCases := []struct {
		name           string
		method         string
		path           string
		expectedType   string
		expectedDigest string
		expectedBody   []byte
	}{
		{
			name:           "manifest by tag",
			method:         http.MethodGet,
			path:           "/v2/dcp/test-image/manifests/latest",
			expectedType:   manifestMediaType,
			expectedDigest: content.manifestDigest,
			expectedBody:   content.manifest,
		},
		{
			name:           "manifest by digest",
			method:         http.MethodGet,
			path:           "/v2/dcp/test-image/manifests/" + content.manifestDigest,
			expectedType:   manifestMediaType,
			expectedDigest: content.manifestDigest,
			expectedBody:   content.manifest,
		},
		{
			name:           "config blob",
			method:         http.MethodGet,
			path:           "/v2/dcp/test-image/blobs/" + content.configDigest,
			expectedType:   configMediaType,
			expectedDigest: content.configDigest,
			expectedBody:   content.config,
		},
		{
			name:           "layer head",
			method:         http.MethodHead,
			path:           "/v2/dcp/test-image/blobs/" + content.layerDigest,
			expectedType:   layerMediaType,
			expectedDigest: content.layerDigest,
			expectedBody:   nil,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			response := httptest.NewRecorder()

			content.ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, "registry/2.0", response.Header().Get("Docker-Distribution-Api-Version"))
			require.Equal(t, testCase.expectedType, response.Header().Get("Content-Type"))
			require.Equal(t, testCase.expectedDigest, response.Header().Get("Docker-Content-Digest"))
			expectedLength := len(testCase.expectedBody)
			if testCase.method == http.MethodHead {
				expectedLength = len(content.layer)
			}
			require.Equal(t, strconv.Itoa(expectedLength), response.Header().Get("Content-Length"))
			require.Equal(t, testCase.expectedBody, response.Body.Bytes())
		})
	}
}

func TestRegistryContentServesVersionCheck(t *testing.T) {
	t.Parallel()

	content, contentErr := newRegistryContent("dcp/test-image", nil)
	require.NoError(t, contentErr)
	request := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	response := httptest.NewRecorder()

	content.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "0", response.Header().Get("Content-Length"))
	require.Equal(t, "registry/2.0", response.Header().Get("Docker-Distribution-Api-Version"))
	require.Empty(t, response.Body.Bytes())
}

func TestRegistryContentRejectsUnknownRequests(t *testing.T) {
	t.Parallel()

	content, contentErr := newRegistryContent("dcp/test-image", nil)
	require.NoError(t, contentErr)

	unknownRequest := httptest.NewRequest(http.MethodGet, "/v2/dcp/test-image/manifests/missing", nil)
	unknownResponse := httptest.NewRecorder()
	content.ServeHTTP(unknownResponse, unknownRequest)
	require.Equal(t, http.StatusNotFound, unknownResponse.Code)

	postRequest := httptest.NewRequest(http.MethodPost, "/v2/", nil)
	postResponse := httptest.NewRecorder()
	content.ServeHTTP(postResponse, postRequest)
	require.Equal(t, http.StatusMethodNotAllowed, postResponse.Code)
	require.Equal(t, "GET, HEAD", postResponse.Header().Get("Allow"))
}

func TestRegistryContentIncludesImageMetadata(t *testing.T) {
	t.Parallel()

	content, contentErr := newRegistryContent("dcp/test-image", map[string]string{"owner": "test"})
	require.NoError(t, contentErr)

	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	configErr := json.Unmarshal(content.config, &config)
	require.NoError(t, configErr)
	require.Equal(t, runtime.GOARCH, config.Architecture)
	require.Equal(t, "linux", config.OS)
	require.Equal(t, map[string]string{"owner": "test"}, config.Config.Labels)

	var manifest imageManifest
	manifestErr := json.Unmarshal(content.manifest, &manifest)
	require.NoError(t, manifestErr)
	require.Equal(t, 2, manifest.SchemaVersion)
	require.Equal(t, content.configDigest, manifest.Config.Digest)
	require.Len(t, manifest.Layers, 1)
	require.Equal(t, content.layerDigest, manifest.Layers[0].Digest)
	require.Equal(t, digest(content.manifest), content.manifestDigest)
}
