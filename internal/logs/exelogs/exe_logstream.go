// Copyright (c) Microsoft Corporation. All rights reserved.

package exelogs

import (
	"context"
	"fmt"
	"io"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/internal/logs"
)

func CreateExecutableLogStream(
	requestCtx context.Context,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	parentKindStorage registry_rest.StandardStorage,
) (io.ReadCloser, error) {
	exe, isExe := obj.(*apiv1.Executable)
	if !isExe {
		return nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Executable): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero()
	if deletionRequested {
		return nil, apierrors.NewBadRequest("Executable is being deleted")
	}

	var logFilePath string

	switch opts.Source {

	case "", string(apiv1.LogStreamSourceStdout):
		if exe.Status.StdOutFile != "" {
			logFilePath = exe.Status.StdOutFile
		} else {
			// TODO: need to wait for the logs to be availabe if opts.Follow is true
			return nil, nil
		}

	case string(apiv1.LogStreamSourceStderr):
		if exe.Status.StdErrFile != "" {
			logFilePath = exe.Status.StdErrFile
		} else {
			// TODO: need to wait for the logs to be availabe if opts.Follow is true
			return nil, nil
		}

	default:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Invalid log source '%s'. Supported log sources are '%s' and '%s'", opts.Source, apiv1.LogStreamSourceStdout, apiv1.LogStreamSourceStderr))
	}

	reader, writer := io.Pipe()
	log := contextdata.GetContextLogger(requestCtx)
	go func() {
		err := logs.WatchLogs(requestCtx, logFilePath, writer, logs.WatchLogOptions{Follow: opts.Follow})
		if err != nil {
			log.Error(err, "Failed to watch Executable logs",
				"Executable", exe.NamespacedName(),
				"LogFilePath", logFilePath,
			)
		}
	}()
	return reader, nil
}
