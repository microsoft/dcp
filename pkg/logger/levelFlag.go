package logger

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	levelStrings = map[string]zapcore.Level{
		"debug": zap.DebugLevel,
		"info":  zap.InfoLevel,
		"error": zap.ErrorLevel,
	}
)

type levelFlagValue struct {
	// This function will be called when we know what the "level enabler" is
	onLevelEnablerAvailable func(zapcore.LevelEnabler)
	value                   string
}

func NewLevelFlagValue(onLevelEnablerAvailable func(zapcore.LevelEnabler)) levelFlagValue {
	return levelFlagValue{
		onLevelEnablerAvailable: onLevelEnablerAvailable,
	}
}

func (lfv *levelFlagValue) Set(flagValue string) error {
	level, namedLevel := levelStrings[strings.ToLower(flagValue)]

	if !namedLevel {
		logLevel, err := strconv.Atoi(flagValue)
		if err != nil {
			return fmt.Errorf("invalid log level \"%s\"", flagValue)
		}

		if logLevel > 0 {
			intLevel := -1 * logLevel // Zap has the levels backwards
			lfv.onLevelEnablerAvailable(zap.NewAtomicLevelAt(zapcore.Level(int8(intLevel))))
		} else {
			return fmt.Errorf("invalid log level \"%s\"", flagValue)
		}
	} else {
		lfv.onLevelEnablerAvailable(zap.NewAtomicLevelAt(level))
	}

	lfv.value = flagValue
	return nil
}

func (lfv *levelFlagValue) String() string {
	return lfv.value
}

func (_ *levelFlagValue) Type() string {
	return "level"
}

var _ pflag.Value = &levelFlagValue{}
