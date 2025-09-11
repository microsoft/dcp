package perftrace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/felixge/fgprof"
	"github.com/go-logr/logr"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

const (
	// Environment variable that enables performance trace capture.
	DCP_PERF_TRACE = "DCP_PERF_TRACE"
)

type ProfileType string

const (
	ProfileTypeStartup     ProfileType = "startup"
	ProfileTypeShutdown    ProfileType = "shutdown"
	ProfileTypeSnapshot    ProfileType = "snapshot"
	ProfileTypeStartupCpu  ProfileType = "startup-cpu"
	ProfileTypeShutdownCpu ProfileType = "shutdown-cpu"
	ProfileTypeSnapshotCpu ProfileType = "snapshot-cpu"
)

var (
	profilingRequests  map[ProfileType]time.Duration
	profilingWaitGroup = &sync.WaitGroup{}
	profilingWaitLock  = &sync.Mutex{}
)

// Waits for any outstanding profiling requests to complete before exiting the program.
// Otherwise the profiling data may not be written to disk. Not necessary in dcp.exe due to
// the way profiling is handled there, but useful for dcpctrl and potentially other
// executables that may use profiling.
func WaitProfilingComplete() {
	// This should only be called at the end of the program after the last chance
	// for profiling to be started, but still adding a lock to ensure a race condition
	// with the profilingWaitGroup is not possible.
	profilingWaitLock.Lock()
	defer profilingWaitLock.Unlock()

	profilingWaitGroup.Wait()
}

func CaptureStartupProfileIfRequested(ctx context.Context, log logr.Logger) error {
	// Attempt to start any requested startup profiling mode.
	return errors.Join(
		captureProfileIfRequested(ctx, ProfileTypeStartup, log),
		captureProfileIfRequested(ctx, ProfileTypeStartupCpu, log),
	)
}

func CaptureShutdownProfileIfRequested(ctx context.Context, log logr.Logger) error {
	// Attempt to start any requested shutdown profiling mode.
	return errors.Join(
		captureProfileIfRequested(ctx, ProfileTypeShutdown, log),
		captureProfileIfRequested(ctx, ProfileTypeShutdownCpu, log),
	)
}

func captureProfileIfRequested(ctx context.Context, pt ProfileType, log logr.Logger) error {
	collectProfilingRequests(log)

	duration, found := profilingRequests[pt]
	if !found {
		return nil // Nothing to do
	}

	profilingCtx, provilingCtxCancel := context.WithTimeout(ctx, duration)
	return StartProfiling(profilingCtx, provilingCtxCancel, pt, log)
}

// Starts profiling the current process till the passed-in context is cancelled.
// The profileType parameter is used as part of the profile data file name, to make it easier to identify
// the correct profile.
func StartProfiling(ctx context.Context, ctxCancel context.CancelFunc, pt ProfileType, log logr.Logger) error {
	programName, err := getCurrentProgramName()
	if err != nil {
		return err
	}

	profileFolder, err := logger.EnsureDiagnosticsLogsFolder()
	if err != nil {
		return err
	}

	// The profile name is <programName>-<profileType>-<timestamp>-<pid>.pprof
	profileFileName := fmt.Sprintf("%s-%s-%d-%d.pprof", programName, pt, time.Now().Unix(), os.Getpid())
	profileOutput, err := usvc_io.OpenFile(filepath.Join(profileFolder, profileFileName), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return fmt.Errorf("failed to create profile file '%s': %w", profileFileName, err)
	}

	switch pt {

	case ProfileTypeStartup, ProfileTypeShutdown, ProfileTypeSnapshot:
		profilingWaitLock.Lock()
		defer profilingWaitLock.Unlock()

		profilingWaitGroup.Add(1)
		stopProfiling := fgprof.Start(profileOutput, fgprof.FormatPprof)

		go func() {
			defer profilingWaitGroup.Done()
			<-ctx.Done()
			ctxCancel() // Release resources associated with the profiling context
			if profilingErr := stopProfiling(); profilingErr != nil {
				log.Error(profilingErr, "Failed to stop profiling", "ProfileFileName", profileFileName)
			}
			if closingErr := profileOutput.Close(); closingErr != nil {
				log.Error(closingErr, "Failed to close profile file", "ProfileFileName", profileFileName)
			}
			log.V(1).Info("Stopped profiling", "Type", pt, "File", profileFileName)
		}()

	case ProfileTypeStartupCpu, ProfileTypeShutdownCpu, ProfileTypeSnapshotCpu:
		profilingWaitLock.Lock()
		defer profilingWaitLock.Unlock()

		profilingWaitGroup.Add(1)
		err = pprof.StartCPUProfile(profileOutput)
		if err != nil {
			return fmt.Errorf("failed to start CPU profiling: %w", err)
		}

		go func() {
			defer profilingWaitGroup.Done()
			<-ctx.Done()
			ctxCancel() // Release resources associated with the profiling context
			pprof.StopCPUProfile()
			if closingErr := profileOutput.Close(); closingErr != nil {
				log.Error(closingErr, "Failed to close profile file", "ProfileFileName", profileFileName)
			}
			log.V(1).Info("Stopped profiling", "Type", pt, "File", profileFileName)
		}()

	default:
		// Should never happen
		return fmt.Errorf("unknown profile type: %s", pt)
	}

	log.V(1).Info("Started profiling", "Type", pt, "File", profileFileName)

	return nil
}

func getCurrentProgramName() (string, error) {
	const errFmt = "could not determine the name of the current executable: %w"

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf(errFmt, err)
	}

	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", fmt.Errorf(errFmt, err)
	}

	exeName := filepath.Base(exePath)
	ext := filepath.Ext(exeName)
	if ext != "" && len(ext) < len(exeName) {
		exeName = exeName[:len(exeName)-len(ext)]
	}
	return exeName, nil
}

// The "profiling requests" come from DCP_PERF_TRACE environment variable, and are in the format:
// request-type=duration,request-type=duration,...
// where request-type is one of "startup" or "shutdown", and duration is a time duration string.
func collectProfilingRequests(log logr.Logger) {
	if profilingRequests != nil {
		return // Already collected
	}

	profilingRequests = make(map[ProfileType]time.Duration)

	requestVar, found := os.LookupEnv(DCP_PERF_TRACE)
	if !found {
		return
	}

	profilingRequests = parseProfilingRequests(requestVar, log)
}

func parseProfilingRequests(requestStr string, log logr.Logger) map[ProfileType]time.Duration {
	retval := make(map[ProfileType]time.Duration)

	requestStr = strings.TrimSpace(requestStr)
	if requestStr == "" {
		return retval
	}

	rawRequests := strings.Split(requestStr, ",")

	for _, rawRequest := range rawRequests {
		requestParts := strings.Split(rawRequest, "=")
		if len(requestParts) != 2 {
			log.Error(fmt.Errorf("invalid profiling request '%s'", rawRequest), "Ignoring profiling request")
			continue
		}

		profileType := ProfileType(strings.TrimSpace(requestParts[0]))
		switch profileType {

		case ProfileTypeStartup, ProfileTypeStartupCpu, ProfileTypeShutdown, ProfileTypeShutdownCpu:
			duration, err := time.ParseDuration(requestParts[1])
			if err != nil {
				log.Error(fmt.Errorf("invalid profiling request '%s' (could not determine the duration)", rawRequest), "Ignoring profiling request")
			} else if duration < time.Second || duration > 5*time.Minute {
				log.Error(fmt.Errorf("invalid profiling request '%s' (duration must be between 1 second and 5 minutes)", rawRequest), "Ignoring profiling request")
			} else {
				retval[profileType] = duration
			}

		default:
			log.Error(fmt.Errorf("invalid profiling request '%s' (unknown profile type)", rawRequest), "Ignoring profiling request")
		}
	}

	return retval
}
