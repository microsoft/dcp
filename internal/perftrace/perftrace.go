package perftrace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
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
	profilingRequests map[ProfileType]time.Duration
)

func CaptureStartupProfileIfRequested(ctx context.Context, log logr.Logger) error {
	return captureProfileIfRequested(ctx, ProfileTypeStartup, log)
}

func CaptureShutdownProfileIfRequested(ctx context.Context, log logr.Logger) error {
	return captureProfileIfRequested(ctx, ProfileTypeShutdown, log)
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

	profileFolder, err := logger.EnsureDetailedLogsFolder()
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
		stopProfiling := fgprof.Start(profileOutput, fgprof.FormatPprof)

		go func() {
			<-ctx.Done()
			ctxCancel() // Release resources associated with the profiling context
			if profilingErr := stopProfiling(); profilingErr != nil {
				log.Error(profilingErr, "failed to stop profiling", "profileFileName", profileFileName)
			}
			if closingErr := profileOutput.Close(); closingErr != nil {
				log.Error(closingErr, "failed to close profile file", "profileFileName", profileFileName)
			}
		}()

	case ProfileTypeStartupCpu, ProfileTypeShutdownCpu, ProfileTypeSnapshotCpu:
		err = pprof.StartCPUProfile(profileOutput)
		if err != nil {
			return fmt.Errorf("failed to start CPU profiling: %w", err)
		}

		go func() {
			<-ctx.Done()
			ctxCancel() // Release resources associated with the profiling context
			pprof.StopCPUProfile()
			if closingErr := profileOutput.Close(); closingErr != nil {
				log.Error(closingErr, "failed to close profile file", "profileFileName", profileFileName)
			}
		}()

	default:
		// Should never happen
		return fmt.Errorf("unknown profile type: %s", pt)
	}

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
			log.Error(fmt.Errorf("invalid profiling request '%s'", rawRequest), "ignoring profiling request")
			continue
		}

		profileType := ProfileType(strings.TrimSpace(requestParts[0]))
		switch profileType {

		case ProfileTypeStartup, ProfileTypeStartupCpu, ProfileTypeShutdown, ProfileTypeShutdownCpu:
			duration, err := time.ParseDuration(requestParts[1])
			if err != nil {
				log.Error(fmt.Errorf("invalid profiling request '%s' (could not determine the duration)", rawRequest), "ignoring profiling request")
			} else if duration < time.Second || duration > 5*time.Minute {
				log.Error(fmt.Errorf("invalid profiling request '%s' (duration must be between 1 second and 5 minutes)", rawRequest), "ignoring profiling request")
			} else {
				retval[profileType] = duration
			}

		default:
			log.Error(fmt.Errorf("invalid profiling request '%s' (unknown profile type)", rawRequest), "ignoring profiling request")
		}
	}

	return retval
}
