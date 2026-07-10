/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/slices"
)

// The types below correspond to data returned by `container ... --format json` commands
// of the Apple container CLI (validated against version 1.0.0). The JSON schema is
// Apple-specific and unrelated to the Docker API schema; definitions only include the
// data DCP cares about.

type appleListedContainer struct {
	Id            string                      `json:"id"`
	Configuration appleContainerConfiguration `json:"configuration"`
	Status        appleContainerStatus        `json:"status"`
}

type appleContainerConfiguration struct {
	Id             string                        `json:"id"`
	CreationDate   time.Time                     `json:"creationDate"`
	Image          appleContainerImage           `json:"image"`
	InitProcess    appleContainerInitProcess     `json:"initProcess"`
	Labels         map[string]string             `json:"labels"`
	Mounts         []appleContainerMount         `json:"mounts"`
	Networks       []appleContainerNetworkConfig `json:"networks"`
	PublishedPorts []appleContainerPublishedPort `json:"publishedPorts"`
}

type appleContainerImage struct {
	Reference  string               `json:"reference"`
	Descriptor appleImageDescriptor `json:"descriptor"`
}

type appleImageDescriptor struct {
	Digest string `json:"digest"`
}

type appleContainerInitProcess struct {
	Arguments        []string `json:"arguments"`
	Environment      []string `json:"environment"`
	Executable       string   `json:"executable"`
	WorkingDirectory string   `json:"workingDirectory"`
}

type appleContainerMount struct {
	Destination string                  `json:"destination"`
	Source      string                  `json:"source"`
	Options     []string                `json:"options"`
	Type        appleContainerMountType `json:"type"`
}

// The mount type is encoded as a single-key object, e.g. {"virtiofs":{}}, {"tmpfs":{}}
// or {"volume":{"name":"...", ...}}.
type appleContainerMountType struct {
	Virtiofs *struct{}                  `json:"virtiofs,omitempty"`
	Tmpfs    *struct{}                  `json:"tmpfs,omitempty"`
	Volume   *appleContainerMountVolume `json:"volume,omitempty"`
}

type appleContainerMountVolume struct {
	Name string `json:"name"`
}

// Note: network option values are not uniformly typed (e.g. "hostname" is a string
// but "mtu" is a number), hence the "any" value type.
type appleContainerNetworkConfig struct {
	Network string         `json:"network"`
	Options map[string]any `json:"options"`
}

type appleContainerPublishedPort struct {
	ContainerPort int32  `json:"containerPort"`
	HostAddress   string `json:"hostAddress"`
	HostPort      int32  `json:"hostPort"`
	Proto         string `json:"proto"`
}

type appleContainerStatus struct {
	State       string                        `json:"state"`
	StartedDate time.Time                     `json:"startedDate"`
	Networks    []appleContainerNetworkStatus `json:"networks"`
}

type appleContainerNetworkStatus struct {
	Network     string `json:"network"`
	Hostname    string `json:"hostname"`
	Ipv4Address string `json:"ipv4Address"`
	Ipv4Gateway string `json:"ipv4Gateway"`
	MacAddress  string `json:"macAddress"`
}

type appleInspectedVolume struct {
	Id            string                   `json:"id"`
	Configuration appleVolumeConfiguration `json:"configuration"`
}

type appleVolumeConfiguration struct {
	CreationDate time.Time         `json:"creationDate"`
	Driver       string            `json:"driver"`
	Labels       map[string]string `json:"labels"`
	Name         string            `json:"name"`
	Source       string            `json:"source"`
}

type appleInspectedImage struct {
	Id            string                  `json:"id"`
	Configuration appleImageConfiguration `json:"configuration"`
	Variants      []appleImageVariant     `json:"variants"`
}

type appleImageConfiguration struct {
	Name       string               `json:"name"`
	Descriptor appleImageDescriptor `json:"descriptor"`
}

type appleImageVariant struct {
	Config appleImageVariantConfig `json:"config"`
}

type appleImageVariantConfig struct {
	Config appleImageVariantInnerConfig `json:"config"`
}

type appleImageVariantInnerConfig struct {
	Labels map[string]string `json:"Labels"`
}

type appleInspectedNetwork struct {
	Id            string                    `json:"id"`
	Configuration appleNetworkConfiguration `json:"configuration"`
	Status        appleNetworkStatus        `json:"status"`
}

type appleNetworkConfiguration struct {
	CreationDate time.Time         `json:"creationDate"`
	Labels       map[string]string `json:"labels"`
	Mode         string            `json:"mode"`
	Name         string            `json:"name"`
	Plugin       string            `json:"plugin"`
}

type appleNetworkStatus struct {
	Ipv4Gateway string `json:"ipv4Gateway"`
	Ipv4Subnet  string `json:"ipv4Subnet"`
	Ipv6Subnet  string `json:"ipv6Subnet"`
}

type appleSystemStatus struct {
	ApiServerVersion string `json:"apiServerVersion"`
	Status           string `json:"status"`
}

// mapContainerState converts an Apple container state into the Docker-compatible
// container status DCP uses. The Apple runtime only reports "running"/"stopped"
// (plus a transient "stopping"); a stopped container that has never been started
// is equivalent to Docker's "created" state.
func mapContainerState(state string, startedDate time.Time) containers.ContainerStatus {
	switch state {
	case "running":
		return containers.ContainerStatusRunning
	case "stopping":
		return containers.ContainerStatusRemoving
	case "stopped":
		if startedDate.IsZero() {
			return containers.ContainerStatusCreated
		}
		return containers.ContainerStatusExited
	default:
		return containers.ContainerStatus(state)
	}
}

// asArrayOfObjects parses command output that contains a single JSON array of objects
// (the format used by `container ... --format json` and `container ... inspect` commands).
func asArrayOfObjects[T any](b *bytes.Buffer, unmarshalFn func(json.RawMessage, *T) error) ([]T, error) {
	if b == nil {
		return nil, fmt.Errorf("the Apple container command timed out without returning any data")
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(b.Bytes(), &rawItems); err != nil {
		return nil, errors.Join(containers.ErrUnmarshalling, err)
	}

	retval := []T{}
	var err error
	for _, raw := range rawItems {
		var obj T
		if unmarshalErr := unmarshalFn(raw, &obj); unmarshalErr != nil {
			err = errors.Join(err, errors.Join(containers.ErrUnmarshalling, unmarshalErr))
		} else {
			retval = append(retval, obj)
		}
	}

	return retval, err
}

func asId(b *bytes.Buffer) (string, error) {
	if b == nil {
		return "", fmt.Errorf("the Apple container command timed out without returning object identifier")
	}

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), []byte("\n")), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

// filterByLabels applies DCP label filters client-side (the Apple container CLI has no
// server-side filtering support).
func filterByLabels[T any](items []T, filters []containers.LabelFilter, getLabels func(T) map[string]string) []T {
	if len(filters) == 0 {
		return items
	}

	filtered := []T{}
	for _, item := range items {
		labels := getLabels(item)
		match := true
		for _, filter := range filters {
			value, found := labels[filter.Key]
			if !found || (filter.Value != "" && value != filter.Value) {
				match = false
				break
			}
		}
		if match {
			filtered = append(filtered, item)
		}
	}

	return filtered
}

func unmarshalDiagnostics(data []byte, info *containers.ContainerDiagnostics) error {
	if data == nil {
		return fmt.Errorf("the Apple container command timed out without returning version data")
	}

	var status appleSystemStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}

	info.ServerVersion = extractVersion(status.ApiServerVersion)

	return nil
}

var versionRegEx = regexp.MustCompile(`version\s+([0-9]\S*)`)

// extractVersion extracts a semantic version from Apple container version strings like
// "container CLI version 1.0.0 (build: release, commit: abcdef)".
func extractVersion(versionString string) string {
	if matches := versionRegEx.FindStringSubmatch(versionString); len(matches) > 1 {
		return matches[1]
	}
	return strings.TrimSpace(versionString)
}

func verifySha256(data []byte, expectedHash string) error {
	hash := sha256.Sum256(data)
	actualHashHex := hex.EncodeToString(hash[:])

	expected := strings.TrimSpace(expectedHash)
	if strings.HasPrefix(strings.ToLower(expected), "sha256:") {
		expected = expected[7:]
	}
	if !strings.EqualFold(actualHashHex, expected) {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedHash, actualHashHex)
	}

	return nil
}

func unmarshalVolume(data json.RawMessage, vol *containers.InspectedVolume) error {
	var aiv appleInspectedVolume
	if err := json.Unmarshal(data, &aiv); err != nil {
		return err
	}

	name := aiv.Configuration.Name
	if name == "" {
		name = aiv.Id
	}

	vol.Name = name
	vol.Driver = aiv.Configuration.Driver
	vol.MountPoint = aiv.Configuration.Source
	vol.Labels = aiv.Configuration.Labels
	vol.CreatedAt = aiv.Configuration.CreationDate

	return nil
}

func unmarshalImage(data json.RawMessage, ic *containers.InspectedImage) error {
	var aii appleInspectedImage
	if err := json.Unmarshal(data, &aii); err != nil {
		return err
	}

	ic.Id = aii.Id
	ic.Tags = []string{aii.Configuration.Name}
	ic.Digest = aii.Configuration.Descriptor.Digest
	for _, variant := range aii.Variants {
		if len(variant.Config.Config.Labels) > 0 {
			ic.Labels = variant.Config.Config.Labels
			break
		}
	}

	return nil
}

func unmarshalListedContainer(data json.RawMessage, lc *containers.ListedContainer) error {
	var alc appleListedContainer
	if err := json.Unmarshal(data, &alc); err != nil {
		return err
	}

	lc.Id = alc.Id
	lc.Name = alc.Id
	lc.Image = alc.Configuration.Image.Reference
	lc.Status = mapContainerState(alc.Status.State, alc.Status.StartedDate)
	lc.Labels = decodeLabels(alc.Configuration.Labels)
	lc.Networks = slices.Map[string](alc.Configuration.Networks, func(n appleContainerNetworkConfig) string { return n.Network })

	return nil
}

func unmarshalContainer(data json.RawMessage, ic *containers.InspectedContainer) error {
	var aic appleListedContainer
	if err := json.Unmarshal(data, &aic); err != nil {
		return err
	}

	ic.Id = aic.Id
	ic.Name = aic.Id
	ic.Image = aic.Configuration.Image.Reference
	ic.CreatedAt = aic.Configuration.CreationDate
	ic.StartedAt = aic.Status.StartedDate
	ic.Status = mapContainerState(aic.Status.State, aic.Status.StartedDate)
	// Note: the Apple container runtime does not report exit codes (or exit errors) of
	// detached containers, so ExitCode is always 0 and Error/FinishedAt remain empty.

	ic.Mounts = make([]apiv1.VolumeMount, 0, len(aic.Configuration.Mounts))
	for _, mount := range aic.Configuration.Mounts {
		readOnly := false
		for _, option := range mount.Options {
			if option == "ro" || option == "readonly" {
				readOnly = true
				break
			}
		}

		switch {
		case mount.Type.Volume != nil:
			ic.Mounts = append(ic.Mounts, apiv1.VolumeMount{
				Type:     apiv1.NamedVolumeMount,
				Source:   mount.Type.Volume.Name,
				Target:   mount.Destination,
				ReadOnly: readOnly,
			})
		case mount.Type.Virtiofs != nil:
			ic.Mounts = append(ic.Mounts, apiv1.VolumeMount{
				Type:     apiv1.BindMount,
				Source:   strings.TrimSuffix(mount.Source, "/"),
				Target:   mount.Destination,
				ReadOnly: readOnly,
			})
		default:
			// Other mount types (e.g. tmpfs) have no DCP equivalent
			continue
		}
	}

	ic.Ports = make(containers.InspectedContainerPortMapping)
	for _, port := range aic.Configuration.PublishedPorts {
		proto := port.Proto
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", port.ContainerPort, strings.ToLower(proto))
		ic.Ports[key] = append(ic.Ports[key], containers.InspectedContainerHostPortConfig{
			HostIp:   port.HostAddress,
			HostPort: fmt.Sprintf("%d", port.HostPort),
		})
	}

	ic.Env = make(map[string]string)
	for _, envVar := range aic.Configuration.InitProcess.Environment {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 1 {
			ic.Env[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			ic.Env[parts[0]] = ""
		}
	}

	if aic.Configuration.InitProcess.Executable != "" {
		ic.Args = append(ic.Args, aic.Configuration.InitProcess.Executable)
	}
	ic.Args = append(ic.Args, aic.Configuration.InitProcess.Arguments...)

	// For a running container the network status includes addressing information.
	// For a stopped (or not yet started) container the status network list is empty,
	// so fall back to the configured networks.
	if len(aic.Status.Networks) > 0 {
		for _, network := range aic.Status.Networks {
			ic.Networks = append(ic.Networks, containers.InspectedContainerNetwork{
				Id:         network.Network,
				Name:       network.Network,
				IPAddress:  stripCidrSuffix(network.Ipv4Address),
				Gateway:    network.Ipv4Gateway,
				MacAddress: network.MacAddress,
				Aliases:    nonEmptyStrings(network.Hostname),
			})
		}
	} else {
		for _, network := range aic.Configuration.Networks {
			hostname, _ := network.Options["hostname"].(string)
			ic.Networks = append(ic.Networks, containers.InspectedContainerNetwork{
				Id:      network.Network,
				Name:    network.Network,
				Aliases: nonEmptyStrings(hostname),
			})
		}
	}

	ic.Labels = decodeLabels(aic.Configuration.Labels)

	return nil
}

func unmarshalNetwork(data json.RawMessage, net *containers.InspectedNetwork) error {
	var ain appleInspectedNetwork
	if err := json.Unmarshal(data, &ain); err != nil {
		return err
	}

	name := ain.Configuration.Name
	if name == "" {
		name = ain.Id
	}

	net.Id = ain.Id
	net.Name = name
	net.CreatedAt = ain.Configuration.CreationDate
	net.Driver = ain.Configuration.Plugin
	net.Labels = ain.Configuration.Labels
	net.IPv6 = ain.Status.Ipv6Subnet != ""
	if ain.Status.Ipv4Subnet != "" {
		net.Subnets = append(net.Subnets, ain.Status.Ipv4Subnet)
		net.Gateways = append(net.Gateways, ain.Status.Ipv4Gateway)
	}
	if ain.Status.Ipv6Subnet != "" {
		net.Subnets = append(net.Subnets, ain.Status.Ipv6Subnet)
	}

	return nil
}

func unmarshalListedNetwork(data json.RawMessage, net *containers.ListedNetwork) error {
	var ain appleInspectedNetwork
	if err := json.Unmarshal(data, &ain); err != nil {
		return err
	}

	name := ain.Configuration.Name
	if name == "" {
		name = ain.Id
	}

	net.Driver = ain.Configuration.Plugin
	net.ID = ain.Id
	net.IPv6 = ain.Status.Ipv6Subnet != ""
	net.Internal = false
	net.Labels = ain.Configuration.Labels
	net.Name = name

	return nil
}

func stripCidrSuffix(address string) string {
	if idx := strings.Index(address, "/"); idx >= 0 {
		return address[:idx]
	}
	return address
}

func nonEmptyStrings(values ...string) []string {
	return slices.NonEmpty[byte](values)
}

// timestampingWriter prefixes every line written through it with an RFC3339Nano timestamp,
// mimicking the output of `docker logs --timestamps` (which the Apple container CLI does
// not support).
type timestampingWriter struct {
	inner       io.Writer
	lock        sync.Mutex
	atLineStart bool
}

func newTimestampingWriter(inner io.Writer) *timestampingWriter {
	return &timestampingWriter{inner: inner, atLineStart: true}
}

func (w *timestampingWriter) Write(p []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	written := 0
	for len(p) > 0 {
		if w.atLineStart {
			timestamp := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := w.inner.Write([]byte(timestamp + " ")); err != nil {
				return written, err
			}
			w.atLineStart = false
		}

		lineEnd := bytes.IndexByte(p, '\n')
		var chunk []byte
		if lineEnd >= 0 {
			chunk = p[:lineEnd+1]
			w.atLineStart = true
		} else {
			chunk = p
		}

		n, err := w.inner.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}

		p = p[len(chunk):]
	}

	return written, nil
}
