/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/microsoft/dcp/internal/pubsub"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
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
	unsupportedHostMessage   = "WSLC is only available on Windows hosts"
)

var (
	nativeWslcVersionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	minimumSupportedWslcVersion = k8s_version.MustParseGeneric(minimumWslcVersion)

	containerNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)container\s+['"].+['"]\s+not found`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("container not found")),
	)
	imageNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)image\s+['"].+['"]\s+not found`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("image not found")),
	)
	networkNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)network not found:\s*['"].+['"]`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("network not found")),
	)
	volumeNotFoundMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)volume not found:\s*['"].+['"]`),
		errors.Join(containers.ErrNotFound, fmt.Errorf("volume not found")),
	)
	alreadyExistsMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(already exists|already in use|already connected|already attached)`),
		containers.ErrAlreadyExists,
	)
	objectInUseMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(is in use|being used|active endpoints|is running|running container)`),
		containers.ErrObjectInUse,
	)
	allocationFailureMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(`(?i)(address pool|could not allocate|no available subnet|port is already allocated|address already in use|failed to bind)`),
		containers.ErrCouldNotAllocate,
	)
	runtimeUnavailableMatch = containers.NewCliErrorMatch(
		regexp.MustCompile(
			`(?i)(`+
				`no active (?:wslc )?(?:default )?session|`+
				`(?:session manager|default session|wslc control endpoint|session control endpoint).*`+
				`(?:not running|unavailable|not found|failed to connect|connection refused)|`+
				`(?:failed to connect|connection refused).*`+
				`(?:session manager|default session|wslc control endpoint|session control endpoint)`+
				`)`,
		),
		containers.ErrRuntimeNotHealthy,
	)
)

type WslcCliOrchestrator struct {
	log      logr.Logger
	executor process.Executor

	cachedStatus    *containers.ContainerRuntimeStatus
	checkStatusLock *concurrency.ContextAwareLock
	statusLock      sync.RWMutex
	statusWorker    atomic.Int32

	containerEvtWatcher *pubsub.SubscriptionSet[containers.EventMessage]
	networkEvtWatcher   *pubsub.SubscriptionSet[containers.EventMessage]
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

func NewWslcCliOrchestrator(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator {
	orchestrator := &WslcCliOrchestrator{
		log:             log,
		executor:        executor,
		checkStatusLock: concurrency.NewContextAwareLock(),
	}
	if runtime.GOOS != "windows" {
		orchestrator.cachedStatus = &containers.ContainerRuntimeStatus{Error: unsupportedHostMessage}
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
	if runtime.GOOS != "windows" {
		return containers.ContainerRuntimeStatus{Error: unsupportedHostMessage}
	}

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

	status := wco.getStatus(ctx)
	wco.storeStatus(status)
	return status
}

func (wco *WslcCliOrchestrator) EnsureBackgroundStatusUpdates(ctx context.Context) {
	if runtime.GOOS != "windows" || ctx.Err() != nil {
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
				status := wco.getStatus(ctx)
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

func (wco *WslcCliOrchestrator) getStatus(ctx context.Context) containers.ContainerRuntimeStatus {
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

	_, networkListErr := wco.listNetworksRaw(ctx, "StatusNetworkList", nil)
	if networkListErr != nil {
		return containers.ContainerRuntimeStatus{
			Installed: true,
			Error:     fmt.Sprintf("checking WSLC network status: %v", networkListErr),
		}
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

func makeWslcCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("wslc", args...)
	if cmd.Path != "" {
		cmd.Args[0] = cmd.Path
	}
	return cmd
}

func (wco *WslcCliOrchestrator) runBufferedWslcCommand(
	ctx context.Context,
	commandName string,
	cmd *exec.Cmd,
	stdoutCloser io.WriteCloser,
	stderrCloser io.WriteCloser,
	timeout time.Duration,
) (*bytes.Buffer, *bytes.Buffer, error) {
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("timeout for WSLC command %q must be positive", commandName)
	}

	effectiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdoutBuffer := new(bytes.Buffer)
	if stdoutCloser == nil {
		cmd.Stdout = stdoutBuffer
	} else {
		defer func() {
			_ = stdoutCloser.Close()
		}()
		cmd.Stdout = io.MultiWriter(stdoutCloser, stdoutBuffer)
	}

	stderrBuffer := new(bytes.Buffer)
	if stderrCloser == nil {
		cmd.Stderr = stderrBuffer
	} else {
		defer func() {
			_ = stderrCloser.Close()
		}()
		cmd.Stderr = io.MultiWriter(stderrCloser, stderrBuffer)
	}
	if contextErr := effectiveCtx.Err(); contextErr != nil {
		return stdoutBuffer, stderrBuffer, contextErr
	}

	wco.log.V(1).Info("Running WSLC command", "Command", cmd.String())
	exitCode, runErr := process.RunToCompletion(effectiveCtx, wco.executor, cmd)
	var commandErr error
	if runErr != nil {
		commandErr = fmt.Errorf("running WSLC command %q: %w", commandName, runErr)
	}
	commandErr = errors.Join(effectiveCtx.Err(), commandErr)
	if exitCode != 0 {
		commandErr = errors.Join(
			commandErr,
			fmt.Errorf("wslc command %q returned non-zero exit code %d", commandName, exitCode),
		)
	}

	return stdoutBuffer, stderrBuffer, commandErr
}

func normalizeCliErrors(errBuf *bytes.Buffer, extraMatches ...containers.ErrorMatch) error {
	matches := append(extraMatches, runtimeUnavailableMatch)
	return containers.NormalizeCliErrors(errBuf, matches...)
}

func asId(b *bytes.Buffer) (string, error) {
	if b == nil {
		return "", fmt.Errorf("the WSLC command timed out without returning object identifier")
	}

	chunks := slices.NonEmpty[byte](slices.Map[[]byte, []byte](bytes.Split(b.Bytes(), osutil.LF()), bytes.TrimSpace))
	if len(chunks) != 1 {
		return "", fmt.Errorf("command output does not contain a single identifier (it is '%s')", b.String())
	}
	return string(chunks[0]), nil
}

func incompleteError(objectKind string, actual int, expected int) error {
	if actual >= expected {
		return nil
	}
	return errors.Join(
		containers.ErrIncomplete,
		fmt.Errorf("only %d out of %d %s were successfully processed", actual, expected, objectKind),
	)
}

func closeWriteCloser(closer io.WriteCloser) {
	if closer != nil {
		_ = closer.Close()
	}
}

var _ containers.ContainerOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.VolumeOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.ImageOrchestrator = (*WslcCliOrchestrator)(nil)
var _ containers.NetworkOrchestrator = (*WslcCliOrchestrator)(nil)
