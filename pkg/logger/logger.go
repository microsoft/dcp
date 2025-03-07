package logger

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	DCP_DIAGNOSTICS_LOG_FOLDER = "DCP_DIAGNOSTICS_LOG_FOLDER" // Folder to write debug logs to (defaults to a temp folder)
	DCP_DIAGNOSTICS_LOG_LEVEL  = "DCP_DIAGNOSTICS_LOG_LEVEL"  // Log level to include in debug logs (defaults to none)
	DCP_LOG_SOCKET             = "DCP_LOG_SOCKET"             // Unix socket to write console logs to instead of stderr

	verbosityFlagName      = "verbosity"
	verbosityFlagShortName = "v"
	stdOutMaxLevel         = zapcore.InfoLevel

	macOsLogErrorFilter = "Could not get process start time, could not read \"/proc\": stat /proc: no such file or directory"
)

var (
	defaultLogPath = filepath.Join(os.TempDir(), "dcp", "logs")
)

type Logger struct {
	logr.Logger
	name        string
	atomicLevel zap.AtomicLevel
	flush       func()
}

type filterSink struct {
	active    bool
	maxLife   int
	filter    string
	innerSink logr.LogSink
	mutex     *sync.Mutex
}

func (fs *filterSink) Init(info logr.RuntimeInfo) {
	fs.innerSink.Init(info)
}

func (fs *filterSink) Enabled(level int) bool {
	return fs.innerSink.Enabled(level)
}

func (fs *filterSink) Info(level int, msg string, keysAndValues ...any) {
	fs.innerSink.Info(level, msg, keysAndValues...)
}

func (fs *filterSink) Error(err error, msg string, keysAndValues ...any) {
	fs.mutex.Lock()
	defer fs.mutex.Unlock()

	if !fs.active {
		fs.innerSink.Error(err, msg, keysAndValues...)
		return
	}

	if msg == fs.filter {
		fs.active = false
		return
	}

	fs.maxLife--

	if fs.maxLife <= 0 {
		fs.active = false
	}

	fs.innerSink.Error(err, msg, keysAndValues...)
}

func (fs *filterSink) WithValues(keysAndValues ...any) logr.LogSink {
	return fs.innerSink.WithValues(keysAndValues...)
}

func (fs *filterSink) WithName(name string) logr.LogSink {
	return fs.innerSink.WithName(name)
}

// New logger implementation to handle logging to stdout/debug log
func New(name string) Logger {
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
			consoleLog = zapcore.AddSync(conn)
		} else {
			fmt.Fprintf(os.Stderr, "the logs should have been written to a Unix domain socket '%s' but an error occcurred when trying to connect: %s\n", dcpLogSocket, err.Error())
		}
	}

	cores := []zapcore.Core{}
	// Add a stderr console logger for log output (with a minimum level set by verbosity)
	cores = append(cores, zapcore.NewCore(consoleEncoder, consoleLog, consoleAtomicLevel))

	// Determine if a debug log is enabled
	if logCore, err := getDiagnosticsLogCore(name, encoderConfig); err != nil {
		// Ignore the error if debug log isn't enabled
		if !errors.Is(err, errDebugLogNotEnabled) {
			// If there was an error setting up the debug log, write it to stderr
			fmt.Fprintf(os.Stderr, "failed to set up debug log: %v\n", err)
		}
	} else {
		// Add the debug log to the list of outputs
		cores = append(cores, logCore)
	}

	zapLogger := zap.New(zapcore.NewTee(cores...))

	logger := zapr.NewLogger(zapLogger).WithName(name)

	if runtime.GOOS == "darwin" {
		innerSink := logger.GetSink()
		logger = logger.WithSink(&filterSink{
			active:    true,
			maxLife:   1,
			filter:    macOsLogErrorFilter,
			innerSink: innerSink,
			mutex:     &sync.Mutex{},
		})
	}

	return Logger{
		Logger:      logger,
		name:        name,
		atomicLevel: consoleAtomicLevel,
		flush: func() {
			_ = zapLogger.Sync()
		},
	}
}

func (l *Logger) SetLevel(level zapcore.Level) {
	l.atomicLevel.SetLevel(level)
}

func (l *Logger) Flush() {
	l.flush()
}

func (l *Logger) BeforeExit(onPanic func(value interface{})) {
	defer l.Flush()

	value := recover()

	l.V(1).Info("exiting")

	if value != nil {
		err := fmt.Errorf("%s panicked: %v", l.name, value)
		l.Error(err, "panic")
		fmt.Fprintln(os.Stderr, err)

		onPanic(value)
	}
}

// Add verbosity flag to enable setting stdout log levels
func (l *Logger) AddLevelFlag(fs *pflag.FlagSet) {
	levelVal := NewLevelFlagValue(func(level zapcore.Level) {
		l.SetLevel(level)
	})
	fs.VarP(&levelVal, verbosityFlagName, verbosityFlagShortName, "Logging verbosity level (e.g. -v=debug). Can be one of 'debug', 'info', or 'error', or any positive integer corresponding to increasing levels of debug verbosity. Levels more than 6 are rarely used in practice.")
}

func getDiagnosticsLogCore(name string, encoderConfig zapcore.EncoderConfig) (zapcore.Core, error) {
	logLevel, err := GetDebugLogLevel()
	if err != nil {
		return nil, err
	}

	logFolder, err := EnsureDetailedLogsFolder()
	if err != nil {
		return nil, err
	}

	// Create a new log file in the output folder with <name>-<timestamp>-<pid> format
	logOutput, err := usvc_io.OpenFile(filepath.Join(logFolder, fmt.Sprintf("%s-%d-%d.log", name, time.Now().Unix(), os.Getpid())), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Format debug log to be machine readible
	logEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// Return a new log core for the debug log
	return zapcore.NewCore(logEncoder, zapcore.AddSync(logOutput), zap.NewAtomicLevelAt(logLevel)), nil
}

// Returns the folder to write detailed logs and perf traces to.
func EnsureDetailedLogsFolder() (string, error) {
	logFolder, found := os.LookupEnv(DCP_DIAGNOSTICS_LOG_FOLDER)
	if !found {
		logFolder = defaultLogPath
	}

	info, err := os.Stat(logFolder)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.MkdirAll(logFolder, osutil.PermissionOnlyOwnerReadWriteSetCurrent); err != nil {
			return "", fmt.Errorf("failed to create the diagnostic log folder '%s': %w", logFolder, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("failed to verify the existence of the diagnostic log folder '%s': %w", logFolder, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("'%s' is not a directory and cannot be used as a log folder", logFolder)
	}

	return logFolder, nil
}

var errDebugLogNotEnabled = errors.New("debug log not enabled")

func GetDebugLogLevel() (zapcore.Level, error) {
	// Determine if the debug log is enabled
	dcpLogLevel, found := os.LookupEnv(DCP_DIAGNOSTICS_LOG_LEVEL)
	if !found {
		return zapcore.InvalidLevel, errDebugLogNotEnabled
	}

	// Parse the debug log level to a zapcore level
	logLevel, err := StringToLevel(dcpLogLevel, zapcore.ErrorLevel)
	if err != nil {
		return zapcore.InvalidLevel, fmt.Errorf("failed to parse log level: %v", dcpLogLevel)
	}

	return logLevel, nil
}
