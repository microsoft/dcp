/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/version"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

// DNS forwarder for network aliases on the Apple container runtime
//
// The Apple container runtime has no network alias support (`--network-alias` does not
// exist) and its gateway DNS serves no records for container names, so names like
// Aspire resource aliases would not resolve between containers. To provide alias
// resolution, DCP runs a small DNS forwarder (cmd/dcpdns) in a container on the default
// network and points every container it creates at it via `--dns`:
//
//   - The forwarder image is built once per DCP version from the dcpdns_c binary that
//     ships alongside dcp (same pattern as the dcptun client proxy image).
//   - One forwarder container runs per DCP instance. It is labeled with the standard
//     creator-process labels, so the resource harvester removes it once the owning DCP
//     process exits.
//   - Records (container names and network aliases -> container IP) are published by
//     atomically rewriting a hosts-format file in a host directory that is bind-mounted
//     read-only into the forwarder; the forwarder re-reads it on change. Queries for
//     unknown names are relayed to the network gateway, so regular resolution still works.
//
// If the forwarder cannot be provisioned (e.g. the image build fails), containers are
// created without `--dns` and keep gateway-only resolution: alias lookups will fail, but
// nothing else regresses.

const (
	// Name of the dcpdns binary shipped alongside the dcp executable.
	dnsBinaryName = "dcpdns_c"

	// Prefix for the DNS forwarder image name (tagged with the DCP version).
	dnsImageNamePrefix = "dcpdns_developer_ms"

	// Path of the dcpdns binary inside the forwarder image.
	dnsBinaryContainerPath = "/usr/local/bin/dcpdns"

	// Mount point of the records directory inside the forwarder container.
	dnsRecordsContainerDir = "/dcp-dns"

	// Name of the hosts-format records file within the records directory.
	dnsRecordsFileName = "hosts"

	// Label marking the forwarder container itself.
	dnsForwarderMarkerLabel = "com.microsoft.developer.usvc-dev.apple-dns-forwarder"

	// Labels consumed by the DCP resource harvester to clean up containers whose creator
	// process has exited. The values must match the constants used by the controllers
	// package (controllers.CreatorProcessIdLabel and friends), which cannot be imported
	// from here without inverting the layering between orchestrators and controllers.
	creatorProcessIdLabel        = "com.microsoft.developer.usvc-dev.creatorProcessId"
	creatorProcessStartTimeLabel = "com.microsoft.developer.usvc-dev.creatorProcessStartTime"
	persistentLabel              = "com.microsoft.developer.usvc-dev.persistent"

	// How long to wait before re-attempting forwarder provisioning after a failure.
	dnsForwarderRetryInterval = time.Minute
)

// dnsForwarderState tracks the per-orchestrator DNS forwarder and its published records.
type dnsForwarderState struct {
	mu sync.Mutex

	// IP address of the running forwarder; empty until successfully provisioned.
	ip string

	// When the last (failed) provisioning attempt finished; zero if none failed yet.
	lastFailedAttempt time.Time

	// Host path of the records file mounted into the forwarder.
	recordsPath string

	// Names (container name + network aliases) to publish for each container once it
	// starts, recorded at creation time.
	pendingNames map[string][]string

	// Currently published records: container name -> (IP, names).
	published map[string]dnsRecord
}

type dnsRecord struct {
	ip    string
	names []string
}

// dnsForwarderContainerName returns the name of this DCP instance's forwarder container.
func dnsForwarderContainerName() string {
	return fmt.Sprintf("dcp-dns-%d", os.Getpid())
}

// ensureDnsForwarder makes sure the DNS forwarder container is running and returns its IP
// address. It returns an empty string when the forwarder is unavailable, in which case
// containers are created without custom DNS (alias resolution is degraded, not fatal).
func (aco *AppleContainerCliOrchestrator) ensureDnsForwarder(ctx context.Context) string {
	aco.dns.mu.Lock()
	defer aco.dns.mu.Unlock()

	if aco.dns.ip != "" {
		return aco.dns.ip
	}
	if !aco.dns.lastFailedAttempt.IsZero() && time.Since(aco.dns.lastFailedAttempt) < dnsForwarderRetryInterval {
		return ""
	}

	ip, provisionErr := aco.provisionDnsForwarderLocked(ctx)
	if provisionErr != nil {
		aco.log.Error(provisionErr, "Could not provision the DNS forwarder container; containers will resolve names via the network gateway only (network aliases will not resolve)")
		aco.dns.lastFailedAttempt = time.Now()
		return ""
	}

	aco.log.V(1).Info("DNS forwarder is running", "Container", dnsForwarderContainerName(), "IP", ip)
	aco.dns.ip = ip
	return ip
}

func (aco *AppleContainerCliOrchestrator) provisionDnsForwarderLocked(ctx context.Context) (string, error) {
	// The upstream resolver for non-alias names is the default network gateway
	// (the resolver containers would use if DCP did not override their DNS).
	defaultNetwork, networkErr := aco.inspectSingleNetwork(ctx, defaultNetworkName)
	if networkErr != nil {
		return "", fmt.Errorf("inspecting the default network: %w", networkErr)
	}
	if len(defaultNetwork.Gateways) == 0 || defaultNetwork.Gateways[0] == "" {
		return "", fmt.Errorf("the default network has no gateway to use as the upstream resolver")
	}
	gateway := defaultNetwork.Gateways[0]

	// Reuse a running forwarder from this DCP instance if one exists (e.g. after the
	// orchestrator is recreated within the same process).
	forwarderName := dnsForwarderContainerName()
	if existing, inspectErr := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{forwarderName}}); inspectErr == nil && len(existing) == 1 {
		if existing[0].Status == containers.ContainerStatusRunning && len(existing[0].Networks) > 0 && existing[0].Networks[0].IPAddress != "" {
			if aco.dns.recordsPath == "" {
				aco.dns.recordsPath = filepath.Join(dnsRecordsHostDir(), dnsRecordsFileName)
			}
			return existing[0].Networks[0].IPAddress, nil
		}

		// Not running (or not usable): remove and recreate below.
		if _, removeErr := aco.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{forwarderName}, Force: true}); removeErr != nil {
			return "", fmt.Errorf("removing stale DNS forwarder container: %w", removeErr)
		}
	}

	imageName, imageErr := aco.ensureDnsImage(ctx)
	if imageErr != nil {
		return "", imageErr
	}

	// Set up the records directory and an initially empty records file.
	recordsDir := dnsRecordsHostDir()
	if mkdirErr := os.MkdirAll(recordsDir, osutil.PermissionOnlyOwnerReadWriteTraverse); mkdirErr != nil {
		return "", fmt.Errorf("creating DNS records directory: %w", mkdirErr)
	}
	recordsPath := filepath.Join(recordsDir, dnsRecordsFileName)
	if writeErr := writeFileAtomically(recordsPath, aco.renderRecordsLocked()); writeErr != nil {
		return "", fmt.Errorf("writing initial DNS records file: %w", writeErr)
	}
	aco.dns.recordsPath = recordsPath

	// Start the forwarder, labeled so the resource harvester removes it when this DCP
	// process exits.
	thisProcess, processErr := process.This()
	if processErr != nil {
		return "", fmt.Errorf("getting current process information: %w", processErr)
	}

	args := []string{
		"run", "--detach",
		"--name", forwarderName,
		"--network", defaultNetworkName,
		"--mount", fmt.Sprintf("type=bind,source=%s,target=%s,readonly", recordsDir, dnsRecordsContainerDir),
		"--label", fmt.Sprintf("%s=true", dnsForwarderMarkerLabel),
		"--label", fmt.Sprintf("%s=%d", creatorProcessIdLabel, thisProcess.Pid),
		"--label", fmt.Sprintf("%s=%s", creatorProcessStartTimeLabel, thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)),
		"--label", fmt.Sprintf("%s=false", persistentLabel),
		imageName,
		dnsBinaryContainerPath,
		"--hosts", dnsRecordsContainerDir + "/" + dnsRecordsFileName,
		"--upstream", gateway,
	}

	cmd := makeAppleContainerCommand(args...)
	outBuf, errBuf, runErr := aco.runBufferedCommand(ctx, "RunDnsForwarder", cmd, nil, nil, defaultRunContainerTimeout)
	if runErr != nil {
		return "", fmt.Errorf("starting the DNS forwarder container: %w", errorsJoinCli(runErr, errBuf))
	}
	if _, idErr := asId(outBuf); idErr != nil {
		return "", fmt.Errorf("starting the DNS forwarder container: %w", idErr)
	}

	// Read back the forwarder's IP address.
	inspected, inspectErr := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{forwarderName}})
	if inspectErr != nil || len(inspected) == 0 || len(inspected[0].Networks) == 0 || inspected[0].Networks[0].IPAddress == "" {
		return "", fmt.Errorf("could not determine the DNS forwarder IP address: %w", inspectErr)
	}

	return inspected[0].Networks[0].IPAddress, nil
}

// ensureDnsImage makes sure the forwarder image exists, building it from the dcpdns_c
// binary that ships alongside the dcp executable if necessary. The image tag encodes the
// DCP version (and, for development builds, the binary hash), so a stale image is never
// reused after an upgrade.
func (aco *AppleContainerCliOrchestrator) ensureDnsImage(ctx context.Context) (string, error) {
	binaryPath, binaryErr := dnsBinaryPath()
	if binaryErr != nil {
		return "", binaryErr
	}

	tag := version.Version().Version
	if tag == version.DevelopmentVersion {
		hash, hashErr := computeFileHash(binaryPath)
		if hashErr != nil {
			return "", fmt.Errorf("computing dcpdns binary hash: %w", hashErr)
		}
		tag += "_" + hash[:12]
	}
	imageName := fmt.Sprintf("%s:%s", dnsImageNamePrefix, tag)

	if existing, inspectErr := aco.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{imageName}}); inspectErr == nil && len(existing) > 0 {
		return imageName, nil
	}

	buildContext, buildContextErr := os.MkdirTemp(usvc_io.DcpTempDir(), "dcpdns-build-*")
	if buildContextErr != nil {
		return "", fmt.Errorf("creating DNS forwarder image build context: %w", buildContextErr)
	}
	defer func() { _ = os.RemoveAll(buildContext) }()

	// Docker-style builds do not follow symlinks outside the build context, so copy the binary in.
	if copyErr := copyFile(binaryPath, filepath.Join(buildContext, dnsBinaryName), osutil.PermissionOnlyOwnerReadWriteExecute); copyErr != nil {
		return "", fmt.Errorf("copying dcpdns binary to build context: %w", copyErr)
	}

	// The dcpdns binary is a static Go executable, so the image needs nothing else.
	dockerfile := fmt.Sprintf("FROM scratch\nCOPY --chmod=0755 %s %s\nCMD [\"%s\"]\n", dnsBinaryName, dnsBinaryContainerPath, dnsBinaryContainerPath)
	dockerfilePath := filepath.Join(buildContext, "Dockerfile")
	if writeErr := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); writeErr != nil {
		return "", fmt.Errorf("writing DNS forwarder Dockerfile: %w", writeErr)
	}

	buildCmd := makeAppleContainerCommand("build", "--progress", "plain", "-f", dockerfilePath, "-t", imageName, buildContext)
	_, errBuf, buildErr := aco.runBufferedCommand(ctx, "BuildDnsForwarderImage", buildCmd, nil, nil, defaultBuildImageTimeout)
	if buildErr != nil {
		return "", fmt.Errorf("building the DNS forwarder image: %w", errorsJoinCli(buildErr, errBuf))
	}

	return imageName, nil
}

// registerPendingDnsNames records the DNS names (container name + network aliases) that
// should be published for a container once it is running. Called at creation time, when
// the aliases are known.
func (aco *AppleContainerCliOrchestrator) registerPendingDnsNames(containerName string, networks []containers.CreateContainerNetworkOptions) {
	if containerName == "" {
		return
	}

	names := []string{containerName}
	seen := map[string]bool{containerName: true}
	for _, network := range networks {
		for _, alias := range network.Aliases {
			if alias != "" && !seen[alias] {
				seen[alias] = true
				names = append(names, alias)
			}
		}
	}

	aco.dns.mu.Lock()
	defer aco.dns.mu.Unlock()
	if aco.dns.pendingNames == nil {
		aco.dns.pendingNames = map[string][]string{}
	}
	aco.dns.pendingNames[containerName] = names
}

// publishDnsRecords resolves the container's IP address and publishes its DNS names to
// the forwarder's records file. Called after a container has been started. Failures are
// logged, not returned: name resolution is best-effort and must not fail container starts.
func (aco *AppleContainerCliOrchestrator) publishDnsRecords(ctx context.Context, containerName string) {
	aco.dns.mu.Lock()
	forwarderRunning := aco.dns.ip != ""
	names := aco.dns.pendingNames[containerName]
	aco.dns.mu.Unlock()

	if !forwarderRunning || containerName == dnsForwarderContainerName() {
		return
	}
	if len(names) == 0 {
		names = []string{containerName}
	}

	inspected, inspectErr := aco.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{containerName}})
	if inspectErr != nil || len(inspected) == 0 || len(inspected[0].Networks) == 0 || inspected[0].Networks[0].IPAddress == "" {
		aco.log.V(1).Info("Could not determine container IP address for DNS records", "Container", containerName)
		return
	}

	aco.dns.mu.Lock()
	defer aco.dns.mu.Unlock()
	if aco.dns.published == nil {
		aco.dns.published = map[string]dnsRecord{}
	}
	aco.dns.published[containerName] = dnsRecord{ip: inspected[0].Networks[0].IPAddress, names: names}
	aco.writeRecordsLocked()
}

// unpublishDnsRecords removes the DNS records for the given containers (called when
// containers are stopped or removed; their IP addresses are no longer valid).
func (aco *AppleContainerCliOrchestrator) unpublishDnsRecords(containerNames []string) {
	aco.dns.mu.Lock()
	defer aco.dns.mu.Unlock()

	changed := false
	for _, name := range containerNames {
		if _, wasPublished := aco.dns.published[name]; wasPublished {
			delete(aco.dns.published, name)
			changed = true
		}
		delete(aco.dns.pendingNames, name)
	}

	if changed {
		aco.writeRecordsLocked()
	}
}

func (aco *AppleContainerCliOrchestrator) writeRecordsLocked() {
	if aco.dns.recordsPath == "" {
		return
	}
	if writeErr := writeFileAtomically(aco.dns.recordsPath, aco.renderRecordsLocked()); writeErr != nil {
		aco.log.Error(writeErr, "Could not update the DNS records file", "Path", aco.dns.recordsPath)
	}
}

func (aco *AppleContainerCliOrchestrator) renderRecordsLocked() []byte {
	var builder strings.Builder
	builder.WriteString("# DNS records published by DCP for the Apple container runtime. Do not edit.\n")

	containerNames := make([]string, 0, len(aco.dns.published))
	for name := range aco.dns.published {
		containerNames = append(containerNames, name)
	}
	sort.Strings(containerNames)

	for _, containerName := range containerNames {
		record := aco.dns.published[containerName]
		builder.WriteString(record.ip)
		for _, name := range record.names {
			builder.WriteString(" ")
			builder.WriteString(name)
		}
		builder.WriteString("\n")
	}

	return []byte(builder.String())
}

func dnsRecordsHostDir() string {
	return filepath.Join(usvc_io.DcpTempDir(), fmt.Sprintf("apple-dns-%d", os.Getpid()))
}

// dnsBinaryPath locates the dcpdns_c binary: next to the running dcp executable in
// installed builds, or under the build output directory in development/test trees.
func dnsBinaryPath() (string, error) {
	dcpDir, dcpDirErr := dcppaths.GetDcpDir()
	if dcpDirErr == nil {
		binaryPath := filepath.Join(dcpDir, dnsBinaryName)
		if info, statErr := os.Stat(binaryPath); statErr == nil && info.Mode().IsRegular() {
			return binaryPath, nil
		}
	}

	// Fallback: probe for the binary in the repository build output (used primarily for testing)
	rootFolder, rootFindErr := osutil.FindRootFor(osutil.FileTarget, dcppaths.BuildOutputDir, dnsBinaryName)
	if rootFindErr != nil {
		return "", fmt.Errorf("dcpdns binary not found next to the running binary and could not be located via filesystem probing: %w", rootFindErr)
	}

	binaryPath := filepath.Join(rootFolder, dcppaths.BuildOutputDir, dnsBinaryName)
	info, statErr := os.Stat(binaryPath)
	if statErr != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("dcpdns binary not found at %s", binaryPath)
	}

	return binaryPath, nil
}

func computeFileHash(path string) (string, error) {
	file, openErr := os.Open(path)
	if openErr != nil {
		return "", openErr
	}
	defer file.Close()

	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, file); copyErr != nil {
		return "", copyErr
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyFile(sourcePath string, destinationPath string, permissions os.FileMode) error {
	source, openErr := os.Open(sourcePath)
	if openErr != nil {
		return openErr
	}
	defer source.Close()

	destination, createErr := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permissions)
	if createErr != nil {
		return createErr
	}

	if _, copyErr := io.Copy(destination, source); copyErr != nil {
		_ = destination.Close()
		return copyErr
	}

	return destination.Close()
}

// errorsJoinCli combines a command error with the normalized CLI error output.
func errorsJoinCli(err error, errBuf *bytes.Buffer) error {
	return errors.Join(err, normalizeCliErrors(errBuf))
}

// writeFileAtomically writes content to a temporary file and renames it over the target,
// so the forwarder never observes a partially written records file.
func writeFileAtomically(path string, content []byte) error {
	tempPath := path + ".tmp"
	if writeErr := os.WriteFile(tempPath, content, 0644); writeErr != nil {
		return writeErr
	}
	return os.Rename(tempPath, path)
}
