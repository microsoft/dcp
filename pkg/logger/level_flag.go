/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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

type LevelFlagValue struct {
	// This function will be called when we know what the "level enabler" is
	onLevelAvailable func(zapcore.Level)
	value            string
}

func NewLevelFlagValue(onLevelAvailable func(zapcore.Level)) LevelFlagValue {
	return LevelFlagValue{
		onLevelAvailable: onLevelAvailable,
	}
}

func StringToLevel(value string, defaultLevel zapcore.Level) (zapcore.Level, error) {
	if level, namedLevel := levelStrings[strings.ToLower(value)]; namedLevel {
		return level, nil
	}

	logLevel, err := strconv.Atoi(value)
	if err != nil {
		return defaultLevel, fmt.Errorf("invalid log level \"%s\"", value)
	}

	if logLevel > 0 {
		intLevel := -1 * logLevel // Zap has the levels backwards
		return zapcore.Level(int8(intLevel)), nil
	} else {
		return defaultLevel, fmt.Errorf("invalid log level \"%s\"", value)
	}
}

func (lfv *LevelFlagValue) Set(flagValue string) error {
	if level, err := StringToLevel(flagValue, zapcore.InfoLevel); err != nil {
		return err
	} else {
		lfv.onLevelAvailable(level)
		lfv.value = flagValue
	}

	return nil
}

func (lfv *LevelFlagValue) String() string {
	return lfv.value
}

func (_ *LevelFlagValue) Type() string {
	return "level"
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

var _ pflag.Value = &LevelFlagValue{}
