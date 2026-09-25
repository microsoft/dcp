/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	"github.com/microsoft/dcp/pkg/process"
)

type serverProxyStartTestExecutor struct {
	process.Executor

	handle      process.ProcessHandle
	startCalls  int
	stopCalls   int
	stopErrors  []error
	outputFiles []string
}

func (executor *serverProxyStartTestExecutor) StartProcess(
	_ context.Context,
	cmd *exec.Cmd,
	_ process.ProcessExitHandler,
	_ process.ProcessCreationFlag,
	_ process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.startCalls++
	for _, output := range []any{cmd.Stdout, cmd.Stderr} {
		if file, isFile := output.(*os.File); isFile {
			executor.outputFiles = append(executor.outputFiles, file.Name())
		}
	}
	return executor.handle, func() {}, nil
}

func (executor *serverProxyStartTestExecutor) StopProcess(
	_ context.Context,
	_ process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	stopIndex := executor.stopCalls
	executor.stopCalls++
	if stopIndex < len(executor.stopErrors) {
		return executor.stopErrors[stopIndex]
	}
	return nil
}

func TestServerProxyConfigFailurePreservesUnconfirmedProcess(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	configReadErr := errors.New("server proxy configuration is unavailable")
	stopErr := errors.New("server proxy stop failed")
	executor := &serverProxyStartTestExecutor{
		handle:     process.NewHandle(4200, time.Unix(1000, 0).UTC()),
		stopErrors: []error{stopErr, nil},
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, configReadErr
			},
		},
	}
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "config-failure",
			UID:  types.UID("config-failure-uid"),
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting)

	started := reconciler.startServerProxy(context.Background(), proxy, proxyData, logr.Discard())

	require.False(t, started)
	require.Equal(t, 1, executor.startCalls)
	require.Equal(t, 1, executor.stopCalls)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.NotNil(t, proxyData.ServerProxyProcessID)
	require.Equal(t, int64(executor.handle.Pid), *proxyData.ServerProxyProcessID)
	require.Equal(t, executor.handle.IdentityTime, proxyData.ServerProxyStartupTimestamp.Time)
	require.Contains(t, proxyData.Message, configReadErr.Error())
	require.Contains(t, proxyData.Message, stopErr.Error())

	retryStopErr := reconciler.stopServerProxyProcess(context.Background(), proxyData)
	require.NoError(t, retryStopErr)
	require.Equal(t, 1, executor.startCalls, "cleanup retry must not launch another server proxy")
	require.Equal(t, 2, executor.stopCalls)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
}

func TestServerProxyConfigFailureRetriesAfterConfirmedCleanup(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4201, time.Unix(1001, 0).UTC()),
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, errors.New("server proxy configuration is unavailable")
			},
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting)

	started := reconciler.startServerProxy(
		context.Background(),
		&apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "confirmed-cleanup",
				UID:  types.UID("confirmed-cleanup-uid"),
			},
		},
		proxyData,
		logr.Discard(),
	)

	require.False(t, started)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateStarting, proxyData.State)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
	require.Equal(t, 1, executor.startCalls)
	require.Equal(t, 1, executor.stopCalls)
}
