// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"k8s.io/apimachinery/pkg/types"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

type LogStreamID uint64

// A map of streams for DCP resources.
// For each resource UID, it holds a bunch of streams represented by follow writer
// for writing resource logs to the destination.
type LogStreamMop map[types.UID]map[LogStreamID]*usvc_io.FollowWriter
