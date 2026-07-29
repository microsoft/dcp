/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hosting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

const hostingTestTimeout = 10 * time.Second

func Test_Host_CanRunWithNoServices(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, hostingTestTimeout)
	defer cancel()

	host := &Host{
		Services: []Service{},
	}

	err := host.Run(ctx, nil)
	require.NoError(t, err)
}

func Test_Host_DetectsDuplicates(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, hostingTestTimeout)
	defer cancel()

	host := &Host{
		Services: []Service{
			NewFuncService("A", func(c context.Context) error { return nil }),
			NewFuncService("A", func(c context.Context) error { return nil }),
		},
	}

	err := host.Run(ctx, nil)
	require.Error(t, err)
}

func Test_Host_RunMultipleServices_HandlesExit(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, hostingTestTimeout)
	defer cancel()

	started := make(chan struct{})

	host := &Host{
		Services: []Service{
			// Different types of exit
			NewFuncService("A", func(c context.Context) error {
				// Early exit
				started <- struct{}{}
				return nil
			}),
			NewFuncService("B", func(c context.Context) error {
				// Graceful exit during shutdown
				started <- struct{}{}
				<-c.Done()
				return nil
			}),
			NewFuncService("C", func(c context.Context) error {
				// Cancellation
				started <- struct{}{}
				<-c.Done()
				return c.Err()
			}),
			NewFuncService("D", func(c context.Context) error {
				// Early-exit error
				started <- struct{}{}
				return errors.New("error from D")
			}),
			NewFuncService("E", func(c context.Context) error {
				// Shutdown error
				started <- struct{}{}
				<-c.Done()
				return errors.New("error from E")
			}),
			NewFuncService("F", func(c context.Context) error {
				// Panic
				started <- struct{}{}
				<-c.Done()
				panic("oh my!")
			}),
		},
	}

	serviceErrors := make(chan LifecycleMessage, len(host.Services))
	stopped := make(chan error)

	// Run the host
	go func() {
		err := host.Run(ctx, serviceErrors)
		stopped <- err
		close(stopped)
		close(started)
	}()

	// Wait for all services to start
	for i := 0; i < len(host.Services); i++ {
		<-started
	}

	// Should have an error from D
	message := <-serviceErrors
	require.Equal(t, "D", message.ServiceName)
	require.Error(t, message.Err)

	// Trigger shutdown - it's not considered a timeout in this case.
	cancel()
	err := <-stopped
	require.NoError(t, err)

	// Could be E or F (order is random)
	message = <-serviceErrors
	require.Regexp(t, "[EF]", message.ServiceName)
	require.Error(t, message.Err)

	message = <-serviceErrors
	require.Regexp(t, "[EF]", message.ServiceName)
	require.Error(t, message.Err)
}

func Test_Host_RunMultipleServices_ShutdownTimeout(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, hostingTestTimeout)
	defer cancel()

	started := make(chan struct{})

	// The two services below stay blocked for the rest of the test binary's life on purpose. Once the
	// host gives up waiting it closes the channel it collects service results on, so a service that
	// finishes afterwards would panic trying to report its result. Do not release them at test cleanup.
	neverTerminates := make(chan struct{})

	host := &Host{
		Services: []Service{
			NewFuncService("A", func(c context.Context) error {
				// Does not terminate
				started <- struct{}{}
				<-c.Done()
				<-neverTerminates
				return nil
			}),
			NewFuncService("B", func(c context.Context) error {
				// Does not terminate
				started <- struct{}{}
				<-c.Done()
				<-neverTerminates
				return nil
			}),
		},
		TimeoutFunc: func() {
			// Allow a timeout to occur immediately after shutdown.
		},
	}

	serviceErrors := make(chan LifecycleMessage, len(host.Services))
	stopped := make(chan error)

	// Run the host
	go func() {
		err := host.Run(ctx, serviceErrors)
		stopped <- err
		close(stopped)
		close(started)
	}()

	// Wait for all services to start
	for i := 0; i < len(host.Services); i++ {
		<-started
	}

	// Trigger shutdown - it's not considered a timeout in this case.
	cancel()
	err := <-stopped
	require.Error(t, err)
	require.Equal(t, "shutdown timeout reached while the following services are still running: A, B", err.Error())
}

func NewFuncService(name string, run func(context.Context) error) Service {
	return &FuncService{name: name, run: run}
}

type FuncService struct {
	name string
	run  func(ctx context.Context) error
}

func (s *FuncService) Name() string {
	return s.name
}

func (s *FuncService) Run(ctx context.Context) error {
	if s.run == nil {
		<-ctx.Done()
		return nil
	}

	return s.run(ctx)
}
