package logger

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	kubezap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	verbosityFlagName = "verbosity"
)

// Create a logger and return it together with a function to flush the output buffers.
// The logger also understands the "-v" logging verbosity level parameter.
func NewLogger(fs *pflag.FlagSet) (logr.Logger, func()) {
	opts := []kubezap.Opts{}

	if fs == nil {
		fs = pflag.NewFlagSet("DCP logger", pflag.ContinueOnError)
		fs.ParseErrorsWhitelist.UnknownFlags = true
	}
	AddLevelFlag(fs, func(le zapcore.LevelEnabler) {
		opts = append(opts, func(o *kubezap.Options) {
			o.Level = le
		})
	})

	var zapLogger *zap.Logger
	err := fs.Parse(os.Args[1:])
	if err == nil {
		zapLogger = kubezap.NewRaw(opts...)
	} else {
		// If we cannot parse the level, we will just take the defaults
		zapLogger = kubezap.NewRaw()
	}
	flushFn := func() {
		_ = zapLogger.Sync() // Best effort
	}
	logger := zapr.NewLogger(zapLogger)
	return logger, flushFn
}

func AddLevelFlag(fs *pflag.FlagSet, onLevelEnablerAvailable func(zapcore.LevelEnabler)) {
	levelVal := NewLevelFlagValue(onLevelEnablerAvailable)
	fs.VarP(&levelVal, verbosityFlagName, "v", "Logging verbosity level (e.g. -v=debug). Can be one of 'debug', 'info', or 'error', or any positive integer corresponding to increasing levels of debug verbosity. Levels more than 6 are rarely used in practice.")
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
