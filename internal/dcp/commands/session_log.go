package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emirpasic/gods/queues/priorityqueue"
	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/spf13/cobra"
)

var (
	outputFileName string
)

func NewSessionLogCommand(log logr.Logger) (*cobra.Command, error) {
	sessionLogCommand := &cobra.Command{
		Use:   "session-log",
		Short: "Outputs a unified session log.",
		Long:  `Outputs a unified log for all diagnostic logs with a given session ID.`,
		RunE:  getSessionLog(log),
		Args:  cobra.ExactArgs(1),
	}

	sessionLogCommand.Flags().StringVarP(&outputFileName, "output", "o", "", "Output file for the session log (default is stdout)")

	return sessionLogCommand, nil
}

func getSessionLog(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log = log.WithName("session-log")

		sessionId := args[0]

		outputWriter := os.Stdout
		if outputFileName != "" {
			outputFile, outputFileErr := usvc_io.OpenFile(outputFileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWrite)
			if outputFileErr != nil {
				log.Error(outputFileErr, "Error opening output file", "File", outputFileName)
				return outputFileErr
			}
			defer outputFile.Close()

			outputWriter = outputFile
		}

		diagnosticsDirectory := logger.GetDiagnosticsLogFolder()
		entries, dirErr := os.ReadDir(diagnosticsDirectory)
		if dirErr != nil {
			return dirErr
		}

		var sessionLogPaths []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			if strings.HasPrefix(entry.Name(), fmt.Sprintf("%s-", sessionId)) {
				sessionLogPaths = append(sessionLogPaths, filepath.Join(diagnosticsDirectory, entry.Name()))
			}
		}

		if len(sessionLogPaths) == 0 {
			err := fmt.Errorf("no logs found for session %s", sessionId)
			log.Error(err, "No logs found", "SessionID", sessionId, "SearchDirectory", diagnosticsDirectory)
			return err
		}

		sessionLogs := make([]*os.File, 0, len(sessionLogPaths))
		// Open all of the session logs
		for _, logFile := range sessionLogPaths {
			f, err := os.OpenFile(logFile, os.O_RDONLY, 0)
			if err != nil {
				log.Error(err, "Error opening session log", "File", logFile)
				return err
			}
			defer f.Close()

			sessionLogs = append(sessionLogs, f)
		}

		if unifyErr := unifyLogs(outputWriter, sessionLogs...); unifyErr != nil {
			log.Error(unifyErr, "Error unifying session logs")
			return unifyErr
		}

		return nil
	}
}

// Custom time type to handle unmarshaling our specific timestamp format
type logTime time.Time

// This is intentionally a no-op, since we only intend to unmarshal log lines, not marshal them
func (lt *logTime) MarshalJSON() ([]byte, error) {
	return []byte{}, nil
}

// Our logs use a custom time format, so we need to implement custom unmarshaling
func (lt *logTime) UnmarshalJSON(data []byte) error {
	// First we need to unmarshal the data into a string
	var s string
	unmarshalErr := json.Unmarshal(data, &s)
	if unmarshalErr != nil {
		return unmarshalErr
	}

	// Then we need to parse the string into a time.Time based on our expected log time format
	parsed, parseErr := time.Parse(logger.LogTimestampFormat, s)
	if parseErr != nil {
		return parseErr
	}

	*lt = logTime(parsed)
	return nil
}

// We only care about the timestamp field in the log line
type timestampedLogLine struct {
	Timestamp logTime `json:"ts"`
}

// logSynchronizer helps read lines from a log file and keeps track of the most recently read line and its timestamp
type logSynchronizer struct {
	// Log file being read
	file *os.File
	// The reader for the file
	buf *bufio.Reader
	// The most recently read line from the log file
	line []byte
	// The timestamp of the most recently read line
	timestamp time.Time
	// Any read error encountered (no more lines after read error)
	err error
}

func newLogSynchronizer(file *os.File) *logSynchronizer {
	ls := &logSynchronizer{
		file: file,
		buf:  bufio.NewReader(file),
	}

	_ = ls.next()

	return ls
}

// Read the next line from the log file and update the timestamp
// If no valid line is found, both line and timestamp will be zeroed out
func (ls *logSynchronizer) next() bool {
	// Invalidate the previous line and timestamp
	ls.line = []byte{}
	ls.timestamp = time.Time{}

	// Read until we have a valid line or an error
	for ls.err == nil && ls.timestamp.IsZero() {
		// Read the next line from the logs to determine the starting timestamp
		line, readErr := ls.buf.ReadBytes('\n')

		if readErr != nil {
			// Record the error, but we'll keep trying to process the line we read (if any)
			ls.err = readErr
		}

		if len(line) > 0 {
			timestampedLine := timestampedLogLine{}
			jsonErr := json.Unmarshal(line, &timestampedLine)
			if jsonErr != nil {
				// Invalid log line format, so record the error (line and timestamp were already zeroed out above)
				ls.err = errors.Join(ls.err, jsonErr)
			} else {
				// Record the line we read and its timestamp
				ls.line = line
				ls.timestamp = time.Time(timestampedLine.Timestamp)
			}
		}
	}

	return !ls.timestamp.IsZero()
}

// Interleaves the provided log files based on log timestamps and writes the results to the provided writer
func unifyLogs(writer io.Writer, logs ...*os.File) error {
	if len(logs) == 0 {
		return fmt.Errorf("no logs provided")
	}

	logSynchronizers := make([]*logSynchronizer, 0, len(logs))
	for _, logFile := range logs {
		logSynchronizers = append(logSynchronizers, newLogSynchronizer(logFile))
	}

	logSyncQueue := priorityqueue.NewWith(func(a, b interface{}) int {
		syncA := a.(*logSynchronizer)
		syncB := b.(*logSynchronizer)
		// First sort by invalid timestamps (nothing remaining to read)
		if !syncA.timestamp.IsZero() && syncB.timestamp.IsZero() {
			return -1
		} else if syncA.timestamp.IsZero() && !syncB.timestamp.IsZero() {
			return 1
		}

		// Then sort by timestamps (earliest first)
		if syncA.timestamp.Before(syncB.timestamp) {
			return -1
		} else if syncA.timestamp.After(syncB.timestamp) {
			return 1
		}

		// Finally sort by filename to ensure a stable sort
		return strings.Compare(syncA.file.Name(), syncB.file.Name())
	})

	for _, ls := range logSynchronizers {
		if !ls.timestamp.IsZero() {
			logSyncQueue.Enqueue(ls)
		}
	}

	for !logSyncQueue.Empty() {
		// We already checked if the queue is empty
		next, _ := logSyncQueue.Dequeue()
		ls := next.(*logSynchronizer)

		_, writeErr := writer.Write(ls.line)
		if writeErr != nil {
			return writeErr
		}

		if ls.next() {
			logSyncQueue.Enqueue(ls)
		}
	}

	var err error
	for _, ls := range logSynchronizers {
		if ls.err != io.EOF {
			err = errors.Join(err, ls.err)
		}
	}

	return err
}
