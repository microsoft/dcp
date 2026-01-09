// Copyright (c) Microsoft Corporation. All rights reserved.

package logger

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	DCP_DIAGNOSTICS_LOG_FOLDER = "DCP_DIAGNOSTICS_LOG_FOLDER" // Folder to write diagnostics logs to (defaults to a temp folder)
	DCP_DIAGNOSTICS_LOG_LEVEL  = "DCP_DIAGNOSTICS_LOG_LEVEL"  // Log level to include in diagnostics logs (defaults to none)
	DCP_LOG_SOCKET             = "DCP_LOG_SOCKET"             // Unix socket to write console logs to instead of stderr
	DCP_LOG_FILE_NAME_SUFFIX   = "DCP_LOG_FILE_NAME_SUFFIX"   // Suffix to append to the log file name (defaults to process ID)
	DCP_LOG_SESSION_ID         = "DCP_LOG_SESSION_ID"         // Session ID to include in log names

	DCP_EPOCH = 1665705600 // DCP epoch is the unix timestamp at the start of the day UTC time of the first commit to the DCP repo (2022-10-14T00:00:00.000Z)

	verbosityFlagName      = "verbosity"
	verbosityFlagShortName = "v"

	// The timestamp format used in logs (ISO8601)
	LogTimestampFormat = "2006-01-02T15:04:05.000Z0700"

	MacOsProcErrorLogFilter = "Could not get process start time, could not read \"/proc\": stat /proc: no such file or directory"
)

var (
	defaultLogPath = filepath.Join(os.TempDir(), "dcp", "logs")
	sessionId      string
)

type Logger struct {
	logr.Logger
	name        string
	atomicLevel zap.AtomicLevel
	flush       func()
}

// New logger implementation to handle logging to stdout/debug log
func New(name string) *Logger {
	// Format console output to be human readable
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// Honor Windows line endings for logs if appropriate
	if runtime.GOOS == "windows" {
		encoderConfig.LineEnding = string(osutil.CRLF())
	}
	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)

	consoleAtomicLevel := zap.NewAtomicLevel()

	// Default to writing logs to stderr
	consoleLog := zapcore.Lock(os.Stderr)

	// If a log socket is set, try to use that instead of stderr
	dcpLogSocket, found := os.LookupEnv(DCP_LOG_SOCKET)
	if found {
		dialer := &net.Dialer{
			Timeout: 2 * time.Second,
		}

		conn, err := dialer.DialContext(context.Background(), "unix", dcpLogSocket)
		if err == nil {
			// If we're able to connect to the socket, use that for console formatted log output
			consoleLog = zapcore.Lock(zapcore.AddSync(conn))
		} else {
			fmt.Fprintf(os.Stderr, "the logs should have been written to a Unix domain socket '%s' but an error occcurred when trying to connect: %s\n", dcpLogSocket, err.Error())
		}
	}

	cores := []zapcore.Core{}
	// Add a stderr console logger for log output (with a minimum level set by verbosity)
	cores = append(cores, zapcore.NewCore(consoleEncoder, consoleLog, consoleAtomicLevel))

	var diagnosticsLogErr error
	// Determine if a diagnostics log is enabled
	if logCore, err := getDiagnosticsLogCore(name, encoderConfig); err != nil {
		// Ignore the error if diagnostics log isn't enabled
		if !errors.Is(err, errDiagnosticsLogNotEnabled) {
			diagnosticsLogErr = err
		}
	} else {
		// Add the diagnostics log to the list of outputs
		cores = append(cores, logCore)
	}

	zapLogger := zap.New(zapcore.NewTee(cores...))

	logger := zapr.NewLogger(zapLogger)

	if diagnosticsLogErr != nil {
		// If there was an error setting up the diagnostics log, write it to the log output and stderr
		logger.Error(diagnosticsLogErr, "Failed to enable diagnostics log output")
		fmt.Fprintf(os.Stderr, "failed to enable diagnostics log output: %v\n", diagnosticsLogErr)
	}

	return &Logger{
		Logger:      logger,
		name:        name,
		atomicLevel: consoleAtomicLevel,
		flush: func() {
			_ = zapLogger.Sync()
		},
	}
}

func (l *Logger) WithName(name string) *Logger {
	l.Logger = l.Logger.WithName(name)
	return l
}

func (l *Logger) WithResourceSink() *Logger {
	resourceSink := newResourceSink(l.atomicLevel, l.Logger.GetSink())
	l.Logger = l.Logger.WithSink(resourceSink)
	flushInner := l.flush
	l.flush = func() {
		flushInner()
		resourceSink.Flush()
	}
	return l
}

func (l *Logger) WithFilterSink(filter string, maxLife uint32) *Logger {
	l.Logger = l.Logger.WithSink(newFilterSink(filter, maxLife, l.Logger.GetSink()))
	return l
}

func (l *Logger) SetLevel(level zapcore.Level) {
	l.atomicLevel.SetLevel(level)
}

func (l *Logger) Flush() {
	l.flush()
}

// Add verbosity flag to enable setting stdout log levels
func (l *Logger) AddLevelFlag(fs *pflag.FlagSet) {
	levelVal := NewLevelFlagValue(func(level zapcore.Level) {
		l.SetLevel(level)
	})
	fs.VarP(&levelVal, verbosityFlagName, verbosityFlagShortName, "Logging verbosity level (e.g. -v=debug). Can be one of 'debug', 'info', or 'error', or any positive integer corresponding to increasing levels of debug verbosity. Levels more than 6 are rarely used in practice.")
}

func getDiagnosticsLogCore(name string, encoderConfig zapcore.EncoderConfig) (zapcore.Core, error) {
	logLevel, err := GetDiagnosticsLogLevel()
	if err != nil {
		return nil, err
	}

	logFolder, err := EnsureDiagnosticsLogsFolder()
	if err != nil {
		return nil, err
	}

	// Create a new log file in the output folder. The default log file name is <session id>-<name>-<process moment hash>
	// but the name can be augmented by setting the DCP_LOG_FILE_NAME_SUFFIX environment variable.
	logFileNameSuffix, found := os.LookupEnv(DCP_LOG_FILE_NAME_SUFFIX)
	if !found || len(logFileNameSuffix) == 0 {
		logFileNameSuffix = ""
	} else {
		logFileNameSuffix = "-" + logFileNameSuffix
	}

	// If custom log file name suffix is used, there's a chance that the file using the resulting name
	// was already created, so let's retry a few times.
	// Worst case we will run without a log file, but that should be super rare.
	b := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(20*time.Millisecond),
		backoff.WithMaxInterval(100*time.Millisecond),
		backoff.WithMaxElapsedTime(2*time.Second),
		backoff.WithRandomizationFactor(0.1),
	)
	logOutput, err := resiliency.RetryGet(context.Background(), b, func() (*os.File, error) {
		logname := fmt.Sprintf("%s-%s-%s%s.log", sessionId, name, ProcessMomentHash(PlainHash), logFileNameSuffix)
		return usvc_io.OpenFile(
			filepath.Join(logFolder, logname),
			os.O_RDWR|os.O_CREATE|os.O_EXCL,
			osutil.PermissionOnlyOwnerReadWrite,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Format debug log to be machine readable
	logEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// Return a new log core for the debug log
	return zapcore.NewCore(logEncoder, zapcore.Lock(logOutput), zap.NewAtomicLevelAt(logLevel)), nil
}

// Returns the folder for diagnostics logs (and perf traces).
func GetDiagnosticsLogFolder() string {
	if logFolder, found := os.LookupEnv(DCP_DIAGNOSTICS_LOG_FOLDER); found {
		return logFolder
	} else {
		return defaultLogPath
	}
}

// Returns the folder to write diagnostics logs and perf traces to.
func EnsureDiagnosticsLogsFolder() (string, error) {
	logFolder := GetDiagnosticsLogFolder()

	info, err := os.Stat(logFolder)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.MkdirAll(logFolder, osutil.PermissionOnlyOwnerReadWriteTraverse); err != nil {
			return "", fmt.Errorf("failed to create the diagnostic log folder '%s': %w", logFolder, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("failed to verify the existence of the diagnostic log folder '%s': %w", logFolder, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("'%s' is not a directory and cannot be used as a log folder", logFolder)
	}

	return logFolder, nil
}

var errDiagnosticsLogNotEnabled = errors.New("diagnostics log not enabled")

func GetDiagnosticsLogLevel() (zapcore.Level, error) {
	// Determine if the diagnostics log is enabled
	diagnosticsLogLevel, found := os.LookupEnv(DCP_DIAGNOSTICS_LOG_LEVEL)
	if !found {
		return zapcore.InvalidLevel, errDiagnosticsLogNotEnabled
	}

	// Parse the diagnostics log level to a zapcore level
	logLevel, err := StringToLevel(diagnosticsLogLevel, zapcore.ErrorLevel)
	if err != nil {
		return zapcore.InvalidLevel, fmt.Errorf("failed to parse log level: %v", diagnosticsLogLevel)
	}

	return logLevel, nil
}

func SessionId() string {
	return sessionId
}

func WithSessionId(cmd *exec.Cmd) {
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", DCP_LOG_SESSION_ID, SessionId()))
}

type MomentHashFlavor int

const (
	PlainHash MomentHashFlavor = iota
	TimeSortableHash
)

// Computes a reasonably-unique, not-too-long value that represents  "this moment in time, from the perspective of the current process".
// It is a hash of current timestamp and current process ID.
func ProcessMomentHash(flavor MomentHashFlavor) string {
	h := fnv.New32a()
	now := time.Now()
	startTime := now.UnixMicro() - DCP_EPOCH*1000*1000
	// Writing to a hash never returns an error, and neither does converting int32/int64 to binary.
	_ = binary.Write(h, binary.NativeEndian, startTime)
	_ = binary.Write(h, binary.NativeEndian, int32(os.Getpid()))
	val := fmt.Sprintf("%x", h.Sum32())
	if flavor == PlainHash {
		return val
	}
	dayHourMinutesSeconds := now.Format("02150405")
	return fmt.Sprintf("%s_%s", dayHourMinutesSeconds, val)
}

func init() {
	if setSessionId, found := os.LookupEnv(DCP_LOG_SESSION_ID); found && setSessionId != "" {
		sessionId = setSessionId
	} else {
		sessionId = ProcessMomentHash(TimeSortableHash)
	}
}
