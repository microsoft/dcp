/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/microsoft/dcp/internal/containers"
)

type wslcListedContainer struct {
	ID       string                     `json:"ID"`
	Names    string                     `json:"Names"`
	Image    string                     `json:"Image"`
	State    containers.ContainerStatus `json:"State"`
	Networks string                     `json:"Networks"`
	Labels   string                     `json:"Labels"`
}

type wslcInspectedContainer struct {
	ID              string                                   `json:"Id"`
	Name            string                                   `json:"Name"`
	Created         time.Time                                `json:"Created"`
	Config          wslcInspectedContainerConfig             `json:"Config"`
	State           wslcInspectedContainerState              `json:"State"`
	Ports           containers.InspectedContainerPortMapping `json:"Ports"`
	Mounts          []wslcInspectedContainerMount            `json:"Mounts"`
	NetworkSettings wslcInspectedContainerNetworkSettings    `json:"NetworkSettings"`
}

type wslcInspectedContainerConfig struct {
	Image       string                                   `json:"Image"`
	Cmd         []string                                 `json:"Cmd"`
	Entrypoint  []string                                 `json:"Entrypoint"`
	Env         []string                                 `json:"Env"`
	Labels      map[string]string                        `json:"Labels"`
	Healthcheck containers.InspectedContainerHealthcheck `json:"Healthcheck"`
}

type wslcInspectedContainerState struct {
	Status     containers.ContainerStatus           `json:"Status"`
	Running    bool                                 `json:"Running"`
	StartedAt  time.Time                            `json:"StartedAt"`
	FinishedAt time.Time                            `json:"FinishedAt"`
	ExitCode   int32                                `json:"ExitCode"`
	Error      string                               `json:"Error"`
	Health     *containers.InspectedContainerHealth `json:"Health"`
}

type wslcInspectedContainerMount struct {
	Type        containers.VolumeMountType `json:"Type"`
	Source      string                     `json:"Source"`
	Destination string                     `json:"Destination"`
	Name        string                     `json:"Name"`
	ReadWrite   bool                       `json:"ReadWrite"`
}

type wslcInspectedContainerNetworkSettings struct {
	Networks map[string]wslcInspectedContainerNetwork `json:"Networks"`
}

type wslcInspectedContainerNetwork struct {
	Aliases    []string `json:"Aliases"`
	Gateway    string   `json:"Gateway"`
	IPAddress  string   `json:"IPAddress"`
	MacAddress string   `json:"MacAddress"`
}

type wslcInspectedImage struct {
	ID          string                   `json:"Id"`
	RepoTags    []string                 `json:"RepoTags"`
	RepoDigests []string                 `json:"RepoDigests"`
	Config      wslcInspectedImageConfig `json:"Config"`
}

type wslcInspectedImageConfig struct {
	Labels map[string]string `json:"Labels"`
}

type wslcListedNetwork struct {
	Driver   string `json:"Driver"`
	ID       string `json:"ID"`
	IPv6     string `json:"IPv6"`
	Internal string `json:"Internal"`
	Labels   string `json:"Labels"`
	Name     string `json:"Name"`
}

type wslcInspectedNetwork struct {
	ID         string                                   `json:"Id"`
	Name       string                                   `json:"Name"`
	Created    time.Time                                `json:"Created"`
	Scope      string                                   `json:"Scope"`
	Driver     string                                   `json:"Driver"`
	EnableIPv6 bool                                     `json:"EnableIPv6"`
	IPv6       bool                                     `json:"IPv6"`
	Internal   bool                                     `json:"Internal"`
	Attachable bool                                     `json:"Attachable"`
	Ingress    bool                                     `json:"Ingress"`
	IPAM       wslcInspectedNetworkIPAM                 `json:"IPAM"`
	Labels     map[string]string                        `json:"Labels"`
	Containers map[string]wslcInspectedNetworkContainer `json:"Containers"`
}

type wslcInspectedNetworkContainer struct {
	Name string `json:"Name"`
}

type wslcInspectedNetworkIPAM struct {
	Config []wslcInspectedNetworkIPAMConfig `json:"Config"`
}

type wslcInspectedNetworkIPAMConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

type wslcListedVolume struct {
	Name string `json:"Name"`
}

type wslcInspectedVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	Mountpoint string            `json:"Mountpoint"`
	Scope      string            `json:"Scope"`
	CreatedAt  time.Time         `json:"CreatedAt"`
}

func decodeJSONLines[T any](buffer *bytes.Buffer) ([]T, error) {
	if buffer == nil {
		return nil, fmt.Errorf("wslc command returned no output buffer")
	}

	results := make([]T, 0)
	var decodeErrors error
	scanner := bufio.NewScanner(bytes.NewReader(buffer.Bytes()))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var result T
		if unmarshalErr := json.Unmarshal(line, &result); unmarshalErr != nil {
			decodeErrors = errors.Join(
				decodeErrors,
				containers.ErrUnmarshalling,
				fmt.Errorf("decoding WSLC JSON line %d: %w", lineNumber, unmarshalErr),
			)
			continue
		}
		results = append(results, result)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		decodeErrors = errors.Join(decodeErrors, containers.ErrUnmarshalling, scanErr)
	}

	return results, decodeErrors
}

func decodeJSONArray[T any](buffer *bytes.Buffer) ([]T, error) {
	if buffer == nil {
		return nil, fmt.Errorf("wslc command returned no output buffer")
	}

	data := bytes.TrimSpace(buffer.Bytes())
	if len(data) == 0 {
		return []T{}, nil
	}

	var rawObjects []json.RawMessage
	if arrayErr := json.Unmarshal(data, &rawObjects); arrayErr != nil {
		return nil, errors.Join(
			containers.ErrUnmarshalling,
			fmt.Errorf("decoding WSLC JSON array: %w", arrayErr),
		)
	}

	results := make([]T, 0, len(rawObjects))
	var decodeErrors error
	for index, rawObject := range rawObjects {
		var result T
		if objectErr := json.Unmarshal(rawObject, &result); objectErr != nil {
			decodeErrors = errors.Join(
				decodeErrors,
				containers.ErrUnmarshalling,
				fmt.Errorf("decoding WSLC JSON object %d: %w", index, objectErr),
			)
			continue
		}
		results = append(results, result)
	}

	return results, decodeErrors
}

func splitCommaSeparated(value string) []string {
	parts := strings.Split(value, ",")
	results := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmedPart := strings.TrimSpace(part)
		if trimmedPart != "" {
			results = append(results, trimmedPart)
		}
	}
	return results
}

func imageDigest(repoDigests []string) string {
	for _, repoDigest := range repoDigests {
		trimmedDigest := strings.TrimSpace(repoDigest)
		if trimmedDigest == "" {
			continue
		}
		if _, digest, found := strings.Cut(trimmedDigest, "@"); found {
			return digest
		}
		return trimmedDigest
	}
	return ""
}
