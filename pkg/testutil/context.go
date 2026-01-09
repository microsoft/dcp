/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	// Set to true to enable advanced networking tests such as those that require access to all network interfaces
	// and try to open ports that are accessible to requests originating from outside of the machine or tests
	// that evaluate network performance against a baseline (these are unreliable on CI machines).
	// This is disabled by default because on Windows it causes a security prompt every time the tests are run.
	DCP_TEST_ENABLE_ADVANCED_NETWORKING = "DCP_TEST_ENABLE_ADVANCED_NETWORKING"

	// Set to true to enable advanced certificate file tests such as those that require openssl installed to
	// verify behavior.
	DCP_TEST_ENABLE_ADVANCED_CERTIFICATES = "DCP_TEST_ENABLE_ADVANCED_CERTIFICATES"

	// Set to true to enable tests that require real container orchestrator (Docker or Podman).
	DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR = "DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR"

	// Used by VS Code to disable skipping tests when they are run with the debugger.
	DCP_TEST_VS_CODE_DEBUGGER = "DCP_TEST_VS_CODE_DEBUGGER"
)

func SkipIfNotEnableAdvancedNetworking(t *testing.T) {
	if osutil.EnvVarSwitchEnabled(DCP_TEST_VS_CODE_DEBUGGER) {
		return
	}
	if osutil.EnvVarSwitchEnabled(DCP_TEST_ENABLE_ADVANCED_NETWORKING) {
		return
	}
	t.Skipf("Skipping test because %s is not set to 'true'", DCP_TEST_ENABLE_ADVANCED_NETWORKING)
}

func SkipIfNotEnabledAdvancedCertificates(t *testing.T) {
	if osutil.EnvVarSwitchEnabled(DCP_TEST_VS_CODE_DEBUGGER) {
		return
	}
	if osutil.EnvVarSwitchEnabled(DCP_TEST_ENABLE_ADVANCED_CERTIFICATES) {
		return
	}
	t.Skipf("Skipping test because %s is not set to 'true'", DCP_TEST_ENABLE_ADVANCED_CERTIFICATES)
}

func SkipIfTrueContainerOrchestratorNotEnabled(t *testing.T) {
	if osutil.EnvVarSwitchEnabled(DCP_TEST_VS_CODE_DEBUGGER) {
		return
	}
	if osutil.EnvVarSwitchEnabled(DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR) {
		return
	}
	t.Skipf("Skipping test because %s is not set to 'true'", DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR)
}

func GetTestContext(t *testing.T, testTimeout time.Duration) (context.Context, context.CancelFunc) {
	timeoutStr, found := os.LookupEnv("TEST_CONTEXT_TIMEOUT")
	if found {
		timeout, err := strconv.ParseUint(timeoutStr, 10, 16)
		if err != nil {
			panic(fmt.Sprintf("Context timeout value '%s' is invalid: %s", timeoutStr, err.Error()))
		}
		return context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	}

	deadline, haveDeadline := t.Deadline()

	switch {
	case !haveDeadline && testTimeout == 0:
		return context.WithCancel(context.Background())

	case haveDeadline && testTimeout == 0:
		return context.WithDeadline(context.Background(), deadline)

	case !haveDeadline && testTimeout != 0:
		return context.WithTimeout(context.Background(), testTimeout)

	case haveDeadline && testTimeout != 0:
		testDeadline := time.Now().Add(testTimeout)
		// Take shorter of the two deadlines
		if testDeadline.Before(deadline) {
			return context.WithDeadline(context.Background(), testDeadline)
		} else {
			return context.WithDeadline(context.Background(), deadline)
		}

	default:
		panic("should never happen--checks above should be exhaustive")
	}
}
