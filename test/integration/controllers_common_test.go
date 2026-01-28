/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"bytes"
	"context"

	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	std_slices "slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgorest "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/networking"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

var (
	testProcessExecutor         *internal_testutil.TestProcessExecutor
	testProcessExecutableRunner *ctrl_testutil.TestProcessExecutableRunner
	ideRunner                   *ctrl_testutil.TestIdeRunner
	client                      ctrl_client.Client
	restClient                  *clientgorest.RESTClient
	containerOrchestrator       *ctrl_testutil.TestContainerOrchestrator
)

const pollImmediately = true // Don't wait before polling for the first time

func TestMain(m *testing.M) {
	log := testutil.NewLogForTesting("IntegrationTests")
	ctrl.SetLogger(log)

	networking.EnableStrictMruPortHandling(log)

	ctx, cancel := context.WithCancel(context.Background())

	serverInfo, teInfo, envStartErr := StartTestEnvironment(ctx, AllControllers, "IntegrationTests", "", log)
	if envStartErr != nil {
		cancel()
		panic(envStartErr)
	}
	client = serverInfo.Client
	restClient = serverInfo.RestClient
	containerOrchestrator = serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	testProcessExecutor = teInfo.TestProcessExecutor
	testProcessExecutableRunner = teInfo.TestProcessExecutableRunner
	ideRunner = teInfo.TestIdeRunner

	var code int = 0
	defer func() {
		cancel()

		// Wait for the API server cleanup to complete. This is mostly about deleting temporary files,
		// so should be relatively quick.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}

		os.Exit(code)
	}()

	code = m.Run()
}

func waitObjectAssumesState[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	t *testing.T,
	ctx context.Context,
	name types.NamespacedName,
	isInState func(*T) (bool, error),
) *T {
	return waitObjectAssumesStateEx[T, PT](t, ctx, client, name, isInState)
}

func waitObjectAssumesStateEx[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	name types.NamespacedName,
	isInState func(*T) (bool, error),
) *T {
	updatedObject, err := commonapi.WaitObjectAssumesState[T, PT](ctx, apiClient, name, isInState)
	if err != nil {
		t.Fatal(err)
	}
	return updatedObject
}

func waitServiceReady(t *testing.T, ctx context.Context, svcName types.NamespacedName) *apiv1.Service {
	return waitServiceReadyEx(t, ctx, client, svcName)
}

func waitServiceReadyEx(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, svcName types.NamespacedName) *apiv1.Service {
	updatedSvc := waitObjectAssumesStateEx(t, ctx, apiClient, svcName, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateReady, nil
	})
	return updatedSvc
}

func retryOnConflict[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context,
	name types.NamespacedName,
	action func(context.Context, PT) error,
) error {
	return retryOnConflictEx(ctx, client, name, action)
}

func retryOnConflictEx[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context,
	apiServerClient ctrl_client.Client,
	name types.NamespacedName,
	action func(context.Context, PT) error,
) error {

	try := func() error {
		var apiObject PT = new(T)
		err := apiServerClient.Get(ctx, name, PT(apiObject))
		if err != nil {
			return resiliency.Permanent(fmt.Errorf("unable to fetch the object '%s' from API server: %w", name.String(), err))
		}

		err = action(ctx, apiObject)
		if apierrors.IsConflict(err) {
			return err // Retry
		} else if err != nil {
			return resiliency.Permanent(err)
		}

		return nil
	}

	return resiliency.RetryExponential(ctx, try)
}

func openLogStream[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context,
	obj PT,
	opts apiv1.LogOptions,
	logStreamOpen *concurrency.AutoResetEvent,
) (io.ReadCloser, error) {
	stream, err := restClient.Get().
		NamespaceIfScoped(obj.GetObjectMeta().Namespace, obj.NamespaceScoped()).
		Resource(obj.GetGroupVersionResource().Resource).
		Name(obj.GetObjectMeta().Name).
		SubResource(apiv1.LogSubresourceName).
		VersionedParams(&opts, apiruntime.NewParameterCodec(dcpclient.NewScheme())).
		Stream(ctx)
	if logStreamOpen != nil {
		logStreamOpen.Set() // Set even if error occurs to unblock the test continuation
	}
	return stream, err
}

func waitForObjectLogs[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context,
	obj PT,
	opts apiv1.LogOptions,
	expectedLines [][]byte,
	logStreamOpen *concurrency.AutoResetEvent,
) error {
	logsArrived := func(receivedLines [][]byte) (bool, error) {
		if len(receivedLines) < len(expectedLines) {
			return false, nil // Not enough data yet
		}

		var matchErr error
		allLinesMatch := std_slices.EqualFunc(receivedLines[len(receivedLines)-len(expectedLines):], expectedLines, func(read, pattern []byte) bool {
			var matched bool
			matched, matchErr = regexp.Match(string(pattern), read)
			return matched
		})
		if matchErr != nil {
			return false, fmt.Errorf("error occurred while matching logs: %w", matchErr)
		}

		return allLinesMatch, nil
	}

	if opts.Follow {
		// In follow mode we open the log stream once and then scan for expected lines
		// until the expectation is satisfied or the context is cancelled.

		logStream, logStreamErr := openLogStream(ctx, obj, opts, logStreamOpen)
		if logStreamErr != nil {
			return fmt.Errorf("could not get log stream %s for %s '%s': %v",
				opts.String(),
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				logStreamErr)
		}

		scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
		var logLines [][]byte

		for scanner.Scan() {
			line := scanner.Bytes()
			logLines = append(logLines, line)

			arrived, err := logsArrived(logLines)
			if err != nil {
				return err
			} else if arrived {
				return nil // Expected logs arrived = success
			}
		}

		if scanner.Err() == nil && ctx.Err() == nil {
			return fmt.Errorf("log stream ended before expected logs arrived for %s '%s'. Logs written so far: %s",
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				string(bytes.Join(logLines, osutil.LineSep())))
		} else {
			return fmt.Errorf("an error occurred while reading logs from %s '%s': %w. Logs written so far: %s",
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				errors.Join(scanner.Err(), ctx.Err()),
				string(bytes.Join(logLines, osutil.LineSep())))
		}
	} else {
		// In non-follow mode we will repeatedly query the logs until we get the expected lines.
		// This deals with the issue that the log stream content may not be available immediately after
		// we simulate writing to the log.

		lastLogContents := []byte{}
		hasExpectedLogLines := func(ctx context.Context) (bool, error) {
			logStream, logStreamErr := openLogStream(ctx, obj, opts, logStreamOpen)
			if logStreamErr != nil {
				return false, fmt.Errorf("could not get log stream %s for %s '%s': %w",
					opts.String(),
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					logStreamErr)
			}
			defer logStream.Close()

			logContents, logReadErr := io.ReadAll(logStream)
			if logReadErr != nil {
				return false, fmt.Errorf("could not read the contents of log stream %s for %s '%s': %w",
					opts.String(),
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					logReadErr)
			}

			lastLogContents = logContents
			logLines := bytes.Split(logContents, osutil.LineSep())
			logLines = std_slices.DeleteFunc(logLines, func(s []byte) bool { return len(s) == 0 }) // Remove empty "lines" from Split() result.

			return logsArrived(logLines)
		}

		err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasExpectedLogLines)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("timeout occurred while waiting for expected logs from %s '%s', last log contents:\n%s\nExpected log contents:\n%s",
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					string(lastLogContents),
					string(bytes.Join(expectedLines, osutil.LineSep())),
				)
			} else {
				return fmt.Errorf("expected logs could not be retrieved: %w", err)
			}
		}
		return nil
	}
}

func generateLogLines(prefix []byte, count int) [][]byte {
	lines := make([][]byte, count)
	for i := 0; i < count; i++ {
		lines[i] = append(prefix, []byte(fmt.Sprintf(" line %d", i+1))...)
	}
	return lines
}

func withTimestampRegexes(lines [][]byte) [][]byte {
	return slices.Map[[]byte, []byte](lines, func(line []byte) []byte {
		return bytes.Join([][]byte{[]byte(osutil.RFC3339MiliTimestampRegex), []byte(" "), line}, nil)
	})
}

func withLineNumberRegexes(lines [][]byte) [][]byte {
	var lineNo uint64 = 1

	return slices.Map[[]byte, []byte](lines, func(line []byte) []byte {
		updated := bytes.Join([][]byte{[]byte(fmt.Sprintf("%d", lineNo)), []byte(" "), line}, nil)
		lineNo++
		return updated
	})
}
