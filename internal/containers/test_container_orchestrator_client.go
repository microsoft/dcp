package containers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type TestContainerOrchestratorClient struct {
	log        logr.Logger
	httpClient *http.Client
}

func NewTestContainerOrchestratorClient(
	lifetimeCtx context.Context,
	log logr.Logger,
	socketFilePath string,
) *TestContainerOrchestratorClient {
	log.V(1).Info("initializing TestContainerOrchestratorClient",
		"socketFilePath", socketFilePath,
	)

	return &TestContainerOrchestratorClient{
		log: log,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketFilePath)
				},
			},
		},
	}
}

func (c *TestContainerOrchestratorClient) CaptureContainerLogs(
	ctx context.Context,
	container string,
	stdout usvc_io.WriteSyncerCloser,
	stderr usvc_io.WriteSyncerCloser,
	options StreamContainerLogsOptions,
) error {
	containerId := strings.TrimSpace(container)
	if containerId == "" {
		return fmt.Errorf("container ID is empty")
	}
	if stdout == nil && stderr == nil {
		return fmt.Errorf("both stdout and stderr log writers are nil")
	}

	type job struct {
		sink   io.WriteCloser
		source apiv1.LogStreamSource
	}
	var jobs []job

	if stdout != nil {
		jobs = append(jobs, job{stdout, apiv1.LogStreamSourceStdout})
	}
	if stderr != nil {
		jobs = append(jobs, job{stderr, apiv1.LogStreamSourceStderr})
	}

	if options.Follow {
		for _, j := range jobs {
			// For now we will just rely on error logging in getLogStream to log errors
			go func() { _ = c.getLogStream(ctx, container, options, j.sink, j.source) }()
		}
		return nil
	} else {
		jobErrors := slices.MapConcurrent[job, error](jobs, func(j job) error {
			return c.getLogStream(ctx, container, options, j.sink, j.source)
		}, slices.MaxConcurrency)
		return errors.Join(jobErrors...)
	}
}

func (c *TestContainerOrchestratorClient) getLogStream(
	ctx context.Context,
	container string,
	options StreamContainerLogsOptions,
	sink io.WriteCloser,
	source apiv1.LogStreamSource,
) error {
	defer sink.Close() // No matter what, we need to tell the sink when no more data is coming.

	traceIdBytes, traceIdErr := randdata.MakeRandomString(8)
	if traceIdErr != nil {
		c.log.V(1).Error(traceIdErr, "could not create a trace ID for container logs request",
			"container", container,
		)
		return fmt.Errorf("could not create a trace ID for request to get logs for container %s: %w", container, traceIdErr)
	}
	traceId := string(traceIdBytes)
	requestLog := c.log.WithValues(
		"container", container,
		"traceId", traceId,
	)
	requestLog.V(1).Info("getting container log stream from test container orchestrator",
		"source", source,
		"follow", options.Follow,
		"timestamps", options.Timestamps,
	)

	url := &url.URL{
		Scheme: "http",
		Host:   "unix", // Does not really matter
	}
	url.Path = fmt.Sprintf(ContainerLogsHttpPath, container)
	query := url.Query()
	query.Set("source", string(source))
	query.Set("follow", fmt.Sprintf("%t", options.Follow))
	query.Set("timestamps", fmt.Sprintf("%t", options.Timestamps))
	url.RawQuery = query.Encode()

	req, reqCreationErr := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if reqCreationErr != nil {
		requestLog.Error(reqCreationErr, "could not create a request to get logs for container")
		return reqCreationErr
	}
	req.Header.Set("trace-id", traceId)

	resp, reqErr := c.httpClient.Do(req)
	if reqErr != nil {
		requestLog.Error(reqErr, "could not get logs for container")
		return reqErr
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		reqErr = fmt.Errorf("request for container logs failed %d %s", resp.StatusCode, resp.Status)
		requestLog.Error(reqErr, "could not get logs for container")
		return reqErr
	}

	copyContext, copyContextCancel := context.WithCancel(ctx)
	defer copyContextCancel()
	bodyReader := usvc_io.NewContextReader(copyContext, resp.Body, true /* leverageReadCloser */)
	written, copyErr := c.copyStream(copyContext, requestLog, sink, bodyReader)
	if copyErr == nil {
		requestLog.V(1).Info("container log stream has been successfully copied")
		return nil
	} else if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		requestLog.V(1).Info("container log stream has been cancelled or timed out")
		return nil
	} else {
		requestLog.Error(copyErr, "could not copy container log stream",
			"totalBytesWritten", written,
		)
		return copyErr
	}
}

var errInvalidWrite = errors.New("invalid write")

func (c *TestContainerOrchestratorClient) copyStream(
	ctx context.Context,
	requestLog logr.Logger,
	sink io.Writer,
	source io.Reader,
) (int, error) {
	var totalWritten int
	buf := make([]byte, 8*1024) // 8 kB should be plenty for our test logs

	for {
		if ctx.Err() != nil {
			requestLog.V(1).Info("log stream context has been cancelled")
			return totalWritten, ctx.Err()
		}

		n, readErr := source.Read(buf)
		if readErr != nil {
			requestLog.Error(readErr, "could not read from log stream source")
			return totalWritten, readErr
		}

		if n == 0 {
			requestLog.V(1).Info("done copying log stream",
				"totalBytesWritten", totalWritten,
			)
			return totalWritten, nil
		}

		content := buf[0:n]

		written, writeErr := sink.Write(content)
		totalWritten += written
		if writeErr != nil {
			requestLog.Error(writeErr, "could not write to log stream sink",
				"totalBytesWritten", totalWritten,
			)
			return totalWritten, writeErr
		}
		if written < len(content) {
			requestLog.Error(io.ErrShortWrite, "some log data was lost when writing to log sink",
				"totalBytesWritten", totalWritten,
				"writtenBytes", written,
				"desiredBytes", len(content),
			)
			return totalWritten, io.ErrShortWrite
		}
		if written > len(content) {
			requestLog.Error(errInvalidWrite, "invalid write occurred when writing to log sink",
				"totalBytesWritten", totalWritten,
				"writtenBytes", written,
				"desiredBytes", len(content),
			)
			return totalWritten, errInvalidWrite
		}
	}
}

var _ ContainerLogSource = (*TestContainerOrchestratorClient)(nil)
