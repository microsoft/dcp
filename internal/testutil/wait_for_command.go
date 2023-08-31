// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	pollImmediately = true // Don't wait before polling for the first time
)

var (
	waitPollInterval = 200 * time.Millisecond
)

// Waits for an execution of a specific command to be issued.
// The command is identified by path to the executable and a subset of its arguments (the first N arguments).
// If lastArg parameter is not empty, it is matched against the last argument of the command.
// Last parameter is a function that is called to verify that the command is the one we are waiting for.
func WaitForCommand(
	executor *TestProcessExecutor,
	ctx context.Context,
	command []string,
	lastArg string,
	cond func(*ProcessExecution) bool,
) (ProcessExecution, error) {
	if len(command) == 0 {
		return ProcessExecution{}, fmt.Errorf("command must not be empty") // Make compiler happy.
	}

	var retval ProcessExecution

	haveExpectedCommand := func(_ context.Context) (bool, error) {
		commands := executor.FindAll(command[0], func(pe ProcessExecution) bool {
			args := pe.Cmd.Args

			if len(args) < len(command) {
				return false // Not enough arguments
			}

			if len(command) > 1 && !slices.StartsWith(args[1:], command[1:]) {
				return false // First N arguments don't match
			}

			if lastArg != "" && args[len(args)-1] != lastArg {
				return false // Last argument doesn't match
			}

			if cond != nil && !cond(&pe) {
				return false // Condition doesn't match
			}

			return true
		})

		if len(commands) == 1 {
			retval = commands[0]
			return true, nil
		} else {
			return false, nil
		}
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, haveExpectedCommand)
	if err != nil {
		return ProcessExecution{}, fmt.Errorf("expected '%v' command (arg: '%s') was not issued: %v", command, lastArg, err)
	}

	return retval, nil
}
