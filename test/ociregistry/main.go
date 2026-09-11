/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// oci-registry is a minimal read-only OCI Distribution v2 registry for container tests.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	configMediaType   = "application/vnd.oci.image.config.v1+json"
	layerMediaType    = "application/vnd.oci.image.layer.v1.tar+gzip"
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	registryPort      = 5000
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type registryContent struct {
	repository     string
	config         []byte
	configDigest   string
	layer          []byte
	layerDigest    string
	manifest       []byte
	manifestDigest string
}

func main() {
	repository := os.Getenv("DCP_REGISTRY_REPOSITORY")
	if repository == "" {
		repository = "dcp/test-image"
	}

	labels := map[string]string{}
	if labelsJSON := os.Getenv("DCP_IMAGE_LABELS"); labelsJSON != "" {
		if labelsErr := json.Unmarshal([]byte(labelsJSON), &labels); labelsErr != nil {
			log.Fatalf("could not parse DCP_IMAGE_LABELS: %v", labelsErr)
		}
	}

	content, contentErr := newRegistryContent(repository, labels)
	if contentErr != nil {
		log.Fatalf("could not generate registry content: %v", contentErr)
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", registryPort),
		Handler:           content,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Serving %s:latest on %s", repository, server.Addr)
	if serveErr := server.ListenAndServe(); serveErr != nil {
		log.Fatalf("registry server failed: %v", serveErr)
	}
}

func newRegistryContent(repository string, labels map[string]string) (*registryContent, error) {
	uncompressedLayer, compressedLayer, layerErr := makeLayer()
	if layerErr != nil {
		return nil, layerErr
	}

	config, configErr := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config": map[string]any{
			"Labels": labels,
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{digest(uncompressedLayer)},
		},
		"history": []map[string]any{{
			"created_by": "dcp test-only in-memory OCI registry",
		}},
	})
	if configErr != nil {
		return nil, fmt.Errorf("marshaling image config: %w", configErr)
	}

	configDigest := digest(config)
	layerDigest := digest(compressedLayer)
	manifest, manifestErr := json.Marshal(imageManifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config: descriptor{
			MediaType: configMediaType,
			Digest:    configDigest,
			Size:      len(config),
		},
		Layers: []descriptor{{
			MediaType: layerMediaType,
			Digest:    layerDigest,
			Size:      len(compressedLayer),
		}},
	})
	if manifestErr != nil {
		return nil, fmt.Errorf("marshaling image manifest: %w", manifestErr)
	}

	return &registryContent{
		repository:     repository,
		config:         config,
		configDigest:   configDigest,
		layer:          compressedLayer,
		layerDigest:    layerDigest,
		manifest:       manifest,
		manifestDigest: digest(manifest),
	}, nil
}

func makeLayer() ([]byte, []byte, error) {
	var uncompressed bytes.Buffer
	tarWriter := tar.NewWriter(&uncompressed)
	contents := []byte("served by the DCP test-only local registry\n")
	if headerErr := tarWriter.WriteHeader(&tar.Header{
		Name: "dcp-local-registry-marker",
		Mode: 0444,
		Size: int64(len(contents)),
	}); headerErr != nil {
		return nil, nil, fmt.Errorf("writing layer header: %w", headerErr)
	}
	if _, contentsErr := tarWriter.Write(contents); contentsErr != nil {
		return nil, nil, fmt.Errorf("writing layer contents: %w", contentsErr)
	}
	if archiveCloseErr := tarWriter.Close(); archiveCloseErr != nil {
		return nil, nil, fmt.Errorf("closing layer archive: %w", archiveCloseErr)
	}

	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, compressErr := gzipWriter.Write(uncompressed.Bytes()); compressErr != nil {
		return nil, nil, fmt.Errorf("compressing layer: %w", compressErr)
	}
	if gzipCloseErr := gzipWriter.Close(); gzipCloseErr != nil {
		return nil, nil, fmt.Errorf("closing compressed layer: %w", gzipCloseErr)
	}
	return uncompressed.Bytes(), compressed.Bytes(), nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (content *registryContent) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if request.URL.Path == "/v2/" {
		response.Header().Set("Content-Length", "0")
		response.WriteHeader(http.StatusOK)
		return
	}

	manifestPrefix := "/v2/" + content.repository + "/manifests/"
	if reference, found := strings.CutPrefix(request.URL.Path, manifestPrefix); found {
		if reference != "latest" && reference != content.manifestDigest {
			http.NotFound(response, request)
			return
		}
		writeContent(response, request, manifestMediaType, content.manifestDigest, content.manifest)
		return
	}

	blobPrefix := "/v2/" + content.repository + "/blobs/"
	if blobDigest, found := strings.CutPrefix(request.URL.Path, blobPrefix); found {
		switch blobDigest {
		case content.configDigest:
			writeContent(response, request, configMediaType, content.configDigest, content.config)
		case content.layerDigest:
			writeContent(response, request, layerMediaType, content.layerDigest, content.layer)
		default:
			http.NotFound(response, request)
		}
		return
	}

	http.NotFound(response, request)
}

func writeContent(
	response http.ResponseWriter,
	request *http.Request,
	mediaType string,
	contentDigest string,
	contents []byte,
) {
	response.Header().Set("Content-Type", mediaType)
	response.Header().Set("Content-Length", strconv.Itoa(len(contents)))
	response.Header().Set("Docker-Content-Digest", contentDigest)
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	if _, responseErr := response.Write(contents); responseErr != nil {
		log.Printf("Could not write response for %s: %v", request.URL.Path, responseErr)
	}
}
