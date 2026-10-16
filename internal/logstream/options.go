/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"errors"
	"fmt"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/slices"
)

type LogStreamSource string

const (
	LogStreamSourceStdout        LogStreamSource = "stdout"
	LogStreamSourceStderr        LogStreamSource = "stderr"
	LogStreamSourceStartupStdout LogStreamSource = "startup_stdout"
	LogStreamSourceStartupStderr LogStreamSource = "startup_stderr"
	LogStreamSourceSystem        LogStreamSource = "system"
)

var (
	validLogStreamSources = []string{
		string(LogStreamSourceStdout),
		string(LogStreamSourceStderr),
		string(LogStreamSourceStartupStdout),
		string(LogStreamSourceStartupStderr),
		string(LogStreamSourceSystem),
	}
)

// LogOptions contains version-neutral log selection and formatting values.
type LogOptions struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// If true, follow the resource's logs.
	// +optional
	Follow bool `json:"follow,omitempty"`

	// The source of the logs to display.
	// Note: the K8s API server does not support non-standard types in query parameters, so we can't use LogStreamSource here.
	// +optional
	Source string `json:"source,omitempty"`

	// If true, include timestamps (RFC3339) in the logs.
	// +optional
	Timestamps bool `json:"timestamps,omitempty"`

	// Limits the number of log lines to return.
	// Cannot be used if Follow option is true.
	// +optional
	Limit *int64 `json:"limit,omitempty"`

	// Limits the response to at most N existing, NEWEST log lines.
	// If Follow is set, new log lines that appear after the log stream was created
	// do not count against the limit, and will be streamed until the client closes the stream.
	// +optional
	Tail *int64 `json:"tail,omitempty"`

	// Skips the first N log lines in the result set. Not compatible with Tail option.
	// +optional
	Skip *int64 `json:"skip,omitempty"`

	// If true, include line numbers in the logs.
	// +optional
	LineNumbers bool `json:"line_numbers,omitempty"`
}

func (lo *LogOptions) String() string {
	return fmt.Sprintf("{Source: %s, Follow: %t, Timestamps: %t, Limit: %s, Skip: %s, Tail: %s, LineNumbers: %t}",
		lo.Source,
		lo.Follow,
		lo.Timestamps,
		logger.IntPtrValToString(lo.Limit),
		logger.IntPtrValToString(lo.Skip),
		logger.IntPtrValToString(lo.Tail),
		lo.LineNumbers,
	)
}

func (lo *LogOptions) Validate() error {
	var retval error

	// Empty source is valid and means "stdout".
	if lo.Source != "" && !slices.Contains(validLogStreamSources, lo.Source) {
		retval = errors.Join(retval, fmt.Errorf("invalid log source '%s'. Supported log sources are '%s', '%s', '%s', '%s', and '%s'", lo.Source, LogStreamSourceStdout, LogStreamSourceStderr, LogStreamSourceStartupStdout, LogStreamSourceStartupStderr, LogStreamSourceSystem))
	}

	if lo.Limit != nil {
		if *lo.Limit <= 0 {
			retval = errors.Join(retval, fmt.Errorf("invalid log response size limit '%d' (must be greater than 0)", *lo.Limit))
		} else if lo.Follow {
			retval = errors.Join(retval, fmt.Errorf("log response size cannot be limited in follow mode"))
		}
	}

	if lo.Tail != nil {
		if *lo.Tail <= 0 {
			retval = errors.Join(retval, fmt.Errorf("invalid log tail value '%d' (must be greater than 0)", *lo.Tail))
		} else if *lo.Tail > usvc_io.MaxTailSize {
			retval = errors.Join(retval, fmt.Errorf("invalid log tail value '%d' (must be less than or equal to %d)", *lo.Tail, usvc_io.MaxTailSize))
		} else if lo.Skip != nil && *lo.Skip > 0 {
			retval = errors.Join(retval, fmt.Errorf("cannot use both 'tail' and 'skip' options"))
		}
	}

	if lo.Skip != nil {
		if *lo.Skip < 0 {
			retval = errors.Join(retval, fmt.Errorf("invalid log skip value '%d' (must be greater than or equal to 0)", *lo.Skip))
		}
	}

	return retval
}

// Normally we would use conversion-gen code generator to generate conversion functions,
// but for some reason this generator is not finding the DeepCopyObject() implementation for this type
// and is failing. Probably a bug. As a workaround we will implement the conversion functions manually.
// We might try harder to use the code generator if we end up having more type conversions
// (e.g. if we support multiple schema versions for DCP).
func UrlValuesToLogOptions(in *url.Values, out *LogOptions, cscope conversion.Scope) error {
	if in == nil {
		return fmt.Errorf("expected a valid net/url.Values object, but got nil")
	}
	if out == nil {
		return fmt.Errorf("expected a valid *LogOptions object as result parameter, but got nil")
	}

	followValues := (*in)["follow"]
	if len(followValues) > 0 {
		if err := runtime.Convert_Slice_string_To_bool(&followValues, &out.Follow, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'follow' parameter: %w", err)
		}
	} else {
		out.Follow = false
	}

	sourceValues := (*in)["source"]
	if len(sourceValues) > 0 {
		if err := runtime.Convert_Slice_string_To_string(&sourceValues, &out.Source, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'source' parameter: %w", err)
		}
	} else {
		out.Source = ""
	}

	timestampsValues := (*in)["timestamps"]
	if len(timestampsValues) > 0 {
		if err := runtime.Convert_Slice_string_To_bool(&timestampsValues, &out.Timestamps, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'timestamps' parameter: %w", err)
		}
	} else {
		out.Timestamps = false
	}

	limitValues := (*in)["limit"]
	if len(limitValues) > 0 {
		if err := runtime.Convert_Slice_string_To_Pointer_int64(&limitValues, &out.Limit, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'limit' parameter: %w", err)
		}
	} else {
		out.Limit = nil
	}

	tailValues := (*in)["tail"]
	if len(tailValues) > 0 {
		if err := runtime.Convert_Slice_string_To_Pointer_int64(&tailValues, &out.Tail, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'tail' parameter: %w", err)
		}
	} else {
		out.Tail = nil
	}

	skipValues := (*in)["skip"]
	if len(skipValues) > 0 {
		if err := runtime.Convert_Slice_string_To_Pointer_int64(&skipValues, &out.Skip, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'skip' parameter: %w", err)
		}
	} else {
		out.Skip = nil
	}

	lineNumbersValues := (*in)["line_numbers"]
	if len(lineNumbersValues) > 0 {
		if err := runtime.Convert_Slice_string_To_bool(&lineNumbersValues, &out.LineNumbers, cscope); err != nil {
			return fmt.Errorf("failed to convert LogOptions 'line_numbers' parameter: %w", err)
		}
	} else {
		out.LineNumbers = false
	}

	return nil
}
