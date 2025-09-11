package containers

import (
	"context"
	"encoding/json"
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
	log.V(1).Info("Initializing TestContainerOrchestratorClient",
		"SocketFilePath", socketFilePath,
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
		c.log.V(1).Error(traceIdErr, "Could not create a trace ID for container logs request",
			"Container", container,
		)
		return fmt.Errorf("could not create a trace ID for request to get logs for container %s: %w", container, traceIdErr)
	}
	traceId := string(traceIdBytes)
	requestLog := c.log.WithValues(
		"Container", container,
		"TraceId", traceId,
	)
	requestLog.V(1).Info("Getting container log stream from test container orchestrator",
		"Source", source,
		"Follow", options.Follow,
		"Timestamps", options.Timestamps,
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
		requestLog.Error(reqCreationErr, "Could not create a request to get logs for container")
		return reqCreationErr
	}
	req.Header.Set("trace-id", traceId)

	resp, reqErr := c.httpClient.Do(req)
	if reqErr != nil {
		requestLog.Error(reqErr, "Could not get logs for container")
		return reqErr
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		reqErr = fmt.Errorf("request for container logs failed %d %s", resp.StatusCode, resp.Status)
		requestLog.Error(reqErr, "Could not get logs for container")
		return reqErr
	}

	copyContext, copyContextCancel := context.WithCancel(ctx)
	defer copyContextCancel()
	bodyReader := usvc_io.NewContextReader(copyContext, resp.Body, true /* leverageReadCloser */)
	written, copyErr := c.copyStream(copyContext, requestLog, sink, bodyReader)
	if copyErr == nil {
		requestLog.V(1).Info("Container log stream has been successfully copied")
		return nil
	} else if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		requestLog.V(1).Info("Container log stream has been cancelled or timed out")
		return nil
	} else {
		requestLog.Error(copyErr, "Could not copy container log stream",
			"TotalBytesWritten", written,
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
			requestLog.V(1).Info("Log stream context has been cancelled")
			return totalWritten, ctx.Err()
		}

		n, readErr := source.Read(buf)
		if readErr != nil {
			requestLog.Error(readErr, "Could not read from log stream source")
			return totalWritten, readErr
		}

		if n == 0 {
			requestLog.V(1).Info("Done copying log stream",
				"TotalBytesWritten", totalWritten,
			)
			return totalWritten, nil
		}

		content := buf[0:n]

		written, writeErr := sink.Write(content)
		totalWritten += written
		if writeErr != nil {
			requestLog.Error(writeErr, "Could not write to log stream sink",
				"TotalBytesWritten", totalWritten,
			)
			return totalWritten, writeErr
		}
		if written < len(content) {
			requestLog.Error(io.ErrShortWrite, "Some log data was lost when writing to log sink",
				"TotalBytesWritten", totalWritten,
				"WrittenBytes", written,
				"DesiredBytes", len(content),
			)
			return totalWritten, io.ErrShortWrite
		}
		if written > len(content) {
			requestLog.Error(errInvalidWrite, "Invalid write occurred when writing to log sink",
				"TotalBytesWritten", totalWritten,
				"WrittenBytes", written,
				"DesiredBytes", len(content),
			)
			return totalWritten, errInvalidWrite
		}
	}
}

func (c *TestContainerOrchestratorClient) InspectContainers(ctx context.Context, options InspectContainersOptions) ([]InspectedContainer, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var results []InspectedContainer
	var allErrors error

	// We typically inspect just one container at a time, so it is fine to loop through them one by one.
	// If we ever need to inspect multiple containers in parallel, we can change this to use goroutines via MapConcurrent().

	for _, containerId := range options.Containers {
		containerId = strings.TrimSpace(containerId)
		if containerId == "" {
			allErrors = errors.Join(allErrors, fmt.Errorf("container ID is empty"))
			continue
		}

		url := &url.URL{
			Scheme: "http",
			Host:   "unix", // Does not really matter
		}
		url.Path = fmt.Sprintf(ContainerHttpPath, containerId)
		query := url.Query()
		query.Set("mode", "inspect")
		url.RawQuery = query.Encode()

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not create request for container %s: %w", containerId, reqErr))
			continue
		}

		resp, reqErr := c.httpClient.Do(req)
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not inspect container %s: %w", containerId, reqErr))
			continue
		}

		switch {

		case resp.StatusCode == http.StatusNotFound:
			allErrors = errors.Join(allErrors, ErrNotFound)

		case resp.StatusCode != http.StatusOK:
			allErrors = errors.Join(allErrors, fmt.Errorf("inspect container %s failed: %d %s", containerId, resp.StatusCode, resp.Status))

		default:
			var container InspectedContainer
			if err := json.NewDecoder(resp.Body).Decode(&container); err != nil {
				allErrors = errors.Join(allErrors, fmt.Errorf("could not decode response for container %s: %w", containerId, err))
			} else {
				results = append(results, container)
			}

		}

		_ = resp.Body.Close()
	}

	if len(results) < len(options.Containers) {
		allErrors = errors.Join(allErrors, errors.Join(ErrIncomplete, fmt.Errorf("not all containers were inspected, expected %d but got %d", len(options.Containers), len(results))))
	}

	return results, allErrors
}

func (c *TestContainerOrchestratorClient) StopContainers(ctx context.Context, options StopContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var results []string
	var allErrors error

	for _, containerId := range options.Containers {
		containerId = strings.TrimSpace(containerId)
		if containerId == "" {
			allErrors = errors.Join(allErrors, fmt.Errorf("container ID is empty"))
			continue
		}

		url := &url.URL{
			Scheme: "http",
			Host:   "unix", // Does not really matter
		}
		url.Path = fmt.Sprintf(ContainerHttpPath, containerId)

		// Create JSON body for merge patch request
		body := map[string]interface{}{
			"status": string(ContainerStatusExited),
		}
		jsonBody, err := json.Marshal(body)
		if err != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not marshal request body for container %s: %w", containerId, err))
			continue
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPatch, url.String(), strings.NewReader(string(jsonBody)))
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not create request for container %s: %w", containerId, reqErr))
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, reqErr := c.httpClient.Do(req)
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not stop container %s: %w", containerId, reqErr))
			continue
		}

		switch {

		case resp.StatusCode == http.StatusNotFound:
			allErrors = errors.Join(allErrors, ErrNotFound)

		case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent:
			allErrors = errors.Join(allErrors, fmt.Errorf("stop container %s failed: %d %s", containerId, resp.StatusCode, resp.Status))

		default:
			results = append(results, containerId)

		}

		_ = resp.Body.Close()
	}

	if len(results) < len(options.Containers) {
		allErrors = errors.Join(allErrors, errors.Join(ErrIncomplete, fmt.Errorf("not all containers were stopped, expected %d but got %d", len(options.Containers), len(results))))
	}

	return results, allErrors
}

func (c *TestContainerOrchestratorClient) RemoveContainers(ctx context.Context, options RemoveContainersOptions) ([]string, error) {
	if len(options.Containers) == 0 {
		return nil, fmt.Errorf("must specify at least one container")
	}

	var results []string
	var allErrors error

	for _, containerId := range options.Containers {
		containerId = strings.TrimSpace(containerId)
		if containerId == "" {
			allErrors = errors.Join(allErrors, fmt.Errorf("container ID is empty"))
			continue
		}

		url := &url.URL{
			Scheme: "http",
			Host:   "unix", // Does not really matter
		}
		url.Path = fmt.Sprintf(ContainerHttpPath, containerId)

		if options.Force {
			query := url.Query()
			query.Set("force", "true")
			url.RawQuery = query.Encode()
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodDelete, url.String(), nil)
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not create request for container %s: %w", containerId, reqErr))
			continue
		}

		resp, reqErr := c.httpClient.Do(req)
		if reqErr != nil {
			allErrors = errors.Join(allErrors, fmt.Errorf("could not remove container %s: %w", containerId, reqErr))
			continue
		}

		switch {

		case resp.StatusCode == http.StatusNotFound:
			allErrors = errors.Join(allErrors, ErrNotFound)

		case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent:
			allErrors = errors.Join(allErrors, fmt.Errorf("remove container %s failed: %d %s", containerId, resp.StatusCode, resp.Status))

		default:
			results = append(results, containerId)
		}

		_ = resp.Body.Close()
	}

	if len(results) < len(options.Containers) {
		allErrors = errors.Join(allErrors, errors.Join(ErrIncomplete, fmt.Errorf("not all containers were removed, expected %d but got %d", len(options.Containers), len(results))))
	}

	return results, allErrors
}

type RemoteContainerOrchestrator interface {
	ContainerLogSource
	InspectContainers
	StopContainers
	RemoveContainers
}

var _ RemoteContainerOrchestrator = (*TestContainerOrchestratorClient)(nil)
