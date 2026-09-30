/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	k8s_version "k8s.io/apimachinery/pkg/util/version"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	ordinaryCommandTimeout   = 30 * time.Second
	versionCommandTimeout    = 2 * time.Second
	diagnosticCommandTimeout = time.Minute
	defaultBuildTimeout      = 10 * time.Minute
	defaultPullTimeout       = 10 * time.Minute
	defaultCreateTimeout     = 10 * time.Minute
	defaultRunTimeout        = 10 * time.Minute
	statusRefreshInterval    = 5 * time.Second
	minimumWslcVersion       = "3.0.1.0"
	defaultWslcContainerHost = "host.wslc.internal"
)

var (
	nativeWslcVersionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	minimumSupportedWslcVersion = k8s_version.MustParseGeneric(minimumWslcVersion)
)

type WslcCliOrchestrator struct {
	log             logr.Logger
	executor        process.Executor
	stopProcessTree stopProcessTreeFunc

	cachedStatus    *containers.ContainerRuntimeStatus
	checkStatusLock *concurrency.ContextAwareLock
	statusLock      sync.RWMutex
	statusWorker    atomic.Int32

	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]
	networkEvtWatcher   *pubsub.SubscriptionSet[containers.EventMessage]
}

func NewWslcCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	orchestrator := &WslcCliOrchestrator{
		log:             log,
		executor:        executor,
		checkStatusLock: concurrency.NewContextAwareLock(),
		stopProcessTree: dcpproc.StopProcessTree,
	}
	orchestrator.containerEvtWatcher = pubsub.NewSubscriptionSet(orchestrator.doWatchContainers, context.Background())
	orchestrator.networkEvtWatcher = pubsub.NewSubscriptionSet(orchestrator.doWatchNetworks, context.Background())
	return orchestrator
}

func (*WslcCliOrchestrator) Name() string {
	return "wslc"
}

func (*WslcCliOrchestrator) ContainerHost() string {
	return defaultWslcContainerHost
}

func (wco *WslcCliOrchestrator) CheckStatus(ctx context.Context, cacheUsage containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	wco.statusLock.RLock()
	if wco.cachedStatus != nil && cacheUsage == containers.CachedRuntimeStatusAllowed {
		status := *wco.cachedStatus
		wco.statusLock.RUnlock()
		return status
	}
	wco.statusLock.RUnlock()

	if cacheUsage == containers.CachedRuntimeStatusAllowed {
		if lockErr := wco.checkStatusLock.Lock(ctx); lockErr != nil {
			return containers.ContainerRuntimeStatus{
				Error: "timed out while checking WSLC status; the WSLC CLI is not responsive",
			}
		}
		defer wco.checkStatusLock.Unlock()

		wco.statusLock.RLock()
		if wco.cachedStatus != nil {
			status := *wco.cachedStatus
			wco.statusLock.RUnlock()
			return status
		}
		wco.statusLock.RUnlock()
	}

	status := wco.getStatusForOS(ctx, runtime.GOOS)
	wco.storeStatus(status)
	return status
}

func (wco *WslcCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if !wco.statusWorker.CompareAndSwap(0, 1) {
		return
	}

	go func() {
		defer wco.statusWorker.Store(0)

		timer := time.NewTimer(0)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}

			if wco.checkStatusLock.TryLock() {
				status := wco.getStatusForOS(ctx, runtime.GOOS)
				wco.storeStatus(status)
				wco.checkStatusLock.Unlock()
			}

			timer.Reset(statusRefreshInterval)
		}
	}()
}

func (wco *WslcCliOrchestrator) storeStatus(status containers.ContainerRuntimeStatus) {
	wco.statusLock.Lock()
	wco.cachedStatus = &status
	wco.statusLock.Unlock()
}

func (wco *WslcCliOrchestrator) getStatusForOS(ctx context.Context, goos string) containers.ContainerRuntimeStatus {
	if goos != "windows" {
		return containers.ContainerRuntimeStatus{
			Error: "WSLC is only available on Windows hosts",
		}
	}

	versionCmd := makeWslcCommand("version", "--format", "json")
	versionOut, versionErrOut, versionRunErr := wco.runBufferedWslcCommand(
		ctx,
		"Version",
		versionCmd,
		nil,
		nil,
		versionCommandTimeout,
	)
	if versionRunErr != nil {
		normalizedErr := errors.Join(versionRunErr, normalizeCliErrors(versionErrOut))
		if errors.Is(normalizedErr, exec.ErrNotFound) {
			return containers.ContainerRuntimeStatus{
				Error: unwrapExecutableNotFound(normalizedErr).Error(),
			}
		}

		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     preferredDiagnostic(normalizedErr, versionErrOut),
		}
	}

	var versionInfo wslcVersion
	if versionDecodeErr := json.Unmarshal(versionOut.Bytes(), &versionInfo); versionDecodeErr != nil {
		return containers.ContainerRuntimeStatus{
			Error: fmt.Sprintf("output from the WSLC version command was invalid: %v", versionDecodeErr),
		}
	}
	if strings.TrimSpace(versionInfo.Client.Version) == "" {
		return containers.ContainerRuntimeStatus{
			Error: "output from the WSLC version command did not contain a client version",
		}
	}
	if versionErr := validateWslcVersion(versionInfo.Client.Version, "client"); versionErr != nil {
		return containers.ContainerRuntimeStatus{Installed: true, Error: versionErr.Error()}
	}

	infoCmd := makeWslcCommand("info", "--format", "json")
	infoOut, infoErrOut, infoRunErr := wco.runBufferedWslcCommand(
		ctx,
		"Info",
		infoCmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if infoRunErr != nil {
		normalizedErr := errors.Join(infoRunErr, normalizeCliErrors(infoErrOut))
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     preferredDiagnostic(normalizedErr, infoErrOut),
		}
	}

	var info wslcInfo
	if infoDecodeErr := json.Unmarshal(infoOut.Bytes(), &info); infoDecodeErr != nil {
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     fmt.Sprintf("output from the WSLC info command was invalid: %v", infoDecodeErr),
		}
	}
	if strings.TrimSpace(info.Client.Version) == "" {
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     "output from the WSLC info command did not contain a client version",
		}
	}
	if strings.TrimSpace(info.Server.SessionManagerVersion) == "" {
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     "output from the WSLC info command did not contain a session manager version",
		}
	}
	if serverVersionErr := validateWslcVersion(info.Server.SessionManagerVersion, "session manager"); serverVersionErr != nil {
		return containers.ContainerRuntimeStatus{Installed: true, Error: serverVersionErr.Error()}
	}
	return containers.ContainerRuntimeStatus{
		Installed: true,
		Running:   true,
	}
}

func validateWslcVersion(version, component string) error {
	normalized := strings.TrimSpace(version)
	parseVersion := k8s_version.ParseSemantic
	if nativeWslcVersionPattern.MatchString(normalized) {
		// WSLC's fourth numeric revision is outside the three-component SemVer format.
		parseVersion = k8s_version.ParseGeneric
	}
	parsedVersion, parseErr := parseVersion(normalized)
	if parseErr != nil {
		return fmt.Errorf("parsing wslc %s version %q: %w", component, version, parseErr)
	}
	// Generic parsing preserves the four-part WSLC minimum; prereleases of that same numeric
	// release still sort below the supported release.
	isUnsupportedPrerelease := parsedVersion.PreRelease() != "" &&
		!parsedVersion.WithPreRelease("").GreaterThan(minimumSupportedWslcVersion)
	if isUnsupportedPrerelease || !parsedVersion.AtLeast(minimumSupportedWslcVersion) {
		return fmt.Errorf("wslc %s version %s is unsupported; DCP requires WSLC %s or newer", component, version, minimumWslcVersion)
	}
	return nil
}

func (wco *WslcCliOrchestrator) GetDiagnostics(ctx context.Context) (containers.ContainerDiagnostics, error) {
	return wco.getDiagnosticsForOS(ctx, runtime.GOOS)
}

func (wco *WslcCliOrchestrator) getDiagnosticsForOS(
	ctx context.Context,
	goos string,
) (containers.ContainerDiagnostics, error) {
	if goos != "windows" {
		return containers.ContainerDiagnostics{}, fmt.Errorf("wslc diagnostics are unavailable on non-Windows hosts")
	}

	cmd := makeWslcCommand("info", "--format", "json")
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"Info",
		cmd,
		nil,
		nil,
		diagnosticCommandTimeout,
	)
	if runErr != nil {
		return containers.ContainerDiagnostics{}, errors.Join(runErr, normalizeCliErrors(errBuf))
	}

	var info wslcInfo
	if decodeErr := json.Unmarshal(outBuf.Bytes(), &info); decodeErr != nil {
		return containers.ContainerDiagnostics{}, fmt.Errorf("decoding WSLC diagnostics: %w", decodeErr)
	}

	clientVersion := strings.TrimSpace(info.Client.Version)
	serverVersion := strings.TrimSpace(info.Server.SessionManagerVersion)
	if clientVersion == "" || serverVersion == "" {
		return containers.ContainerDiagnostics{}, fmt.Errorf(
			"wslc diagnostics are incomplete: client version %q, session manager version %q",
			clientVersion,
			serverVersion,
		)
	}

	return containers.ContainerDiagnostics{
		ClientVersion: clientVersion,
		ServerVersion: serverVersion,
	}, nil
}

func unwrapExecutableNotFound(err error) error {
	currentErr := err
	for currentErr != nil {
		unwrappedErr := errors.Unwrap(currentErr)
		if unwrappedErr == nil || !errors.Is(unwrappedErr, exec.ErrNotFound) {
			break
		}
		currentErr = unwrappedErr
	}
	return currentErr
}

func preferredDiagnostic(runErr error, errBuf interface{ String() string }) string {
	if errBuf != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if stderr != "" {
			return stderr
		}
	}
	if runErr == nil {
		return ""
	}
	return runErr.Error()
}

type wslcVersion struct {
	Client wslcClientInfo `json:"Client"`
}

type wslcInfo struct {
	Client wslcClientInfo `json:"Client"`
	Server wslcServerInfo `json:"Server"`
}

type wslcClientInfo struct {
	Version string `json:"Version"`
}

type wslcServerInfo struct {
	SessionManagerVersion string        `json:"SessionManagerVersion"`
	Sessions              []wslcSession `json:"Sessions"`
}

type wslcSession struct {
	ID   int64  `json:"ID"`
	Name string `json:"Name"`
}

var _ containers.ContainerOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.VolumeOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.ImageOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*WslcCliOrchestrator)(nil)
