/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package flags

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/pflag"
)

const RuntimeFlagName = "container-runtime"

type RuntimeFlagValue string

const (
	UnknownRuntime RuntimeFlagValue = ""
	DockerRuntime  RuntimeFlagValue = "docker"
	PodmanRuntime  RuntimeFlagValue = "podman"
)

var (
	supportedRuntimeNames = []string{"docker", "podman"}
	runtime               = UnknownRuntime
)

func EnsureRuntimeFlag(flags *pflag.FlagSet) {
	flags.Var(&runtime, RuntimeFlagName, fmt.Sprintf("The container runtime to use (%s)", strings.Join(supportedRuntimeNames, ", ")))
}

func GetRuntimeFlag() string {
	return "--" + RuntimeFlagName
}

func GetRuntimeFlagValue() RuntimeFlagValue {
	return runtime
}

func (rf *RuntimeFlagValue) Set(flagValue string) error {
	if flagValue == string(UnknownRuntime) || slices.ContainsFunc(supportedRuntimeNames, func(name string) bool {
		return name == strings.ToLower(flagValue)
	}) {
		*rf = RuntimeFlagValue(strings.ToLower(flagValue))
		return nil
	}

	return fmt.Errorf("container runtime \"%s\" is invalid, must be one of (%s)", flagValue, strings.Join(supportedRuntimeNames, ", "))
}

func (rf *RuntimeFlagValue) String() string {
	return string(*rf)
}

func (*RuntimeFlagValue) Type() string {
	return RuntimeFlagName
}
