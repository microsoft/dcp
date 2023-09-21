package logger

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	DCP_LOG_FOLDER         = "DCP_LOG_FOLDER"     // Folder to write debug logs to (defaults to a temp folder)
	DCP_LOG_LEVEL          = "DCP_LOG_LEVEL"      // Log level to include in debug logs (defaults to none)
	DCP_LOG_SOCKET         = "DCP_LOG_SOCKET"     // Unix socket to write console logs to instead of stderr
	DCP_SESSION_FOLDER     = "DCP_SESSION_FOLDER" // Folder to delete when finished with a session
	verbosityFlagName      = "verbosity"
	verbosityFlagShortName = "v"
	stdOutMaxLevel         = zapcore.InfoLevel
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

type debugLogNotEnabledError struct {
	err string
}

func newDebugLogNotEnabledError(err string) *debugLogNotEnabledError {
	return &debugLogNotEnabledError{
		err: err,
	}
}

func (e *debugLogNotEnabledError) Error() string {
	return e.err
}

func isDebugLogNotEnabledError(err error) bool {
	_, ok := err.(*debugLogNotEnabledError)
	return ok
}

func getLogCore(name string, encoderConfig zapcore.EncoderConfig) (zapcore.Core, error) {
	// Determine if the debug log is enabled
	dcpLogLevel, found := os.LookupEnv(DCP_LOG_LEVEL)
	if !found {
		return nil, newDebugLogNotEnabledError("debug log not enabled")
	}

	// Parse the debug log level to a zapcore level
	logLevel, err := StringToLevel(dcpLogLevel, zapcore.ErrorLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to parse log level: %v", dcpLogLevel)
	}

	// Determine the folder to write debug logs to
	logFolder, found := os.LookupEnv(DCP_LOG_FOLDER)
	if !found {
		logFolder = defaultLogPath
	}

	// Attempt to create the relevant folder
	if err := os.MkdirAll(logFolder, os.FileMode(0700)); err != nil {
		return nil, fmt.Errorf("failed to create log folder: %v", err)
	}

	// Create a new log file in the output folder with <name>-<timestamp>-<pid> format
	logOutput, err := os.Create(filepath.Join(logFolder, fmt.Sprintf("%s-%d-%d", name, time.Now().Unix(), os.Getpid())))
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %v", err)
	}

	// Format debug log to be machine readible
	logEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// Return a new log core for the debug log
	return zapcore.NewCore(logEncoder, zapcore.AddSync(logOutput), zap.NewAtomicLevelAt(logLevel)), nil
}

// New logger implementation to handle logging to stdout/debug log
func New(name string) Logger {
	cores := []zapcore.Core{}

	// Format console output to be human readible
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// Honor Windows line endings for logs if appropriate
	if runtime.GOOS == "windows" {
		encoderConfig.LineEnding = "\r\n"
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

		if conn, err := dialer.DialContext(context.Background(), "unix", dcpLogSocket); err == nil {
			// If we're able to connect to the socket, use that for console formatted log output
			consoleLog = zapcore.AddSync(conn)
		}
	}
	// Add a stderr console logger for log output (with a minimum level set by verbosity)
	cores = append(cores, zapcore.NewCore(consoleEncoder, consoleLog, consoleAtomicLevel))

	// Determine if a debug log is enabled
	if logCore, err := getLogCore(name, zap.NewProductionEncoderConfig()); err != nil {
		// Ignore the error if debug log isn't enabled
		if !isDebugLogNotEnabledError(err) {
			// If there was an error setting up the debug log, write it to stderr
			fmt.Fprintf(os.Stderr, "failed to set up debug log: %v\n", err)
		}
	} else {
		// Add the debug log to the list of outputs
		cores = append(cores, logCore)
	}

	zapLogger := zap.New(zapcore.NewTee(cores...))

	return Logger{
		Logger:      zapr.NewLogger(zapLogger),
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

func GetLevelFlagValue(fs *pflag.FlagSet) (*LevelFlagValue, bool) {
	if fs == nil {
		return nil, false
	}

	levelFlag := fs.Lookup(verbosityFlagName)
	if levelFlag == nil {
		return nil, false
	}

	levelVal, ok := levelFlag.Value.(*LevelFlagValue)
	if !ok {
		return nil, false
	}

	return levelVal, true
}

func GetVerbosityArg(fs *pflag.FlagSet) string {
	if levelFlagValue, found := GetLevelFlagValue(fs); found && levelFlagValue.String() != "" {
		return fmt.Sprintf("-v=%s", levelFlagValue.String())
	} else {
		return ""
	}
}

var shouldCleanupSessionFolder bool = true

func PreserveSessionFolder() {
	shouldCleanupSessionFolder = false
}

func CleanupSessionFolderIfNeeded() {
	if !shouldCleanupSessionFolder {
		return
	}

	dcpSessionDir, found := os.LookupEnv(DCP_SESSION_FOLDER)
	if found {
		if err := os.RemoveAll(dcpSessionDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove session directory: %v\n", err)
		}
	}
}
