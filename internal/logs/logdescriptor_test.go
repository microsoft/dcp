// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	timeWithMilliseconds = "15:04:05.000"
)

func TestLogFollowingDelayWithinBounds(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	log := logger.New("test")
	log.SetLevel(zapcore.ErrorLevel)
	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}
	lds := NewLogDescriptorSet(ctx, testutil.TestTempRoot(), log.Logger)
	uid, err := randdata.MakeRandomString(8)
	require.NoError(t, err)

	ld, stdoutWriter, _, created, err := lds.AcquireForResource(ctx, cancel,
		types.NamespacedName{
			Namespace: metav1.NamespaceNone,
			Name:      "test-log-following-delay-within-bounds",
		},
		types.UID(uid),
	)
	require.NoError(t, err)
	require.True(t, created)
	defer lds.ReleaseForResource(types.UID(uid))

	stdOutPath, _, err := ld.LogConsumerStarting()
	require.NoError(t, err)
	defer ld.LogConsumerStopped()

	// Make 8 writes at the span of 4 * logReadRetryInterval.
	buf := testutil.NewBufferWriter()
	const numWrites = 8
	const writeDelay = 4 * logReadRetryInterval / numWrites
	watcherDone := make(chan struct{})

	go func() {
		watcherErr := WatchLogs(ctx, stdOutPath, buf, WatchLogOptions{Follow: true})
		require.NoError(t, watcherErr)
		ld.LogConsumerStopped()
		close(watcherDone)
	}()

	var logWriteTimes []time.Time
	for i := 0; i < numWrites; i++ {
		logWriteTimes = append(logWriteTimes, time.Now())
		content := []byte(fmt.Sprintf("%d\n", i))
		_, writeErr := stdoutWriter.Write(content)
		require.NoError(t, writeErr)
		time.Sleep(writeDelay)
	}

	// Wait for all writes to be captured.
	const waitPollInterval = 50 * time.Millisecond
	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		content := buf.Bytes()
		lines := bytes.Count(content, []byte("\n"))
		return lines == numWrites, nil
	})
	require.NoError(t, err)

	stdoutWriter.Close()
	cancel() // Stop the watcher
	<-watcherDone

	// Each write should be captured after no more that the sum of:
	// - logReadRetryInterval (how long we wait before checking if log file has new data)
	// - waitPollInterval (how often we check if all writes have been captured in this test)
	// - 100 ms (extra delay to account for timing imprecisions and enhance test robustness)
	const maxDiscrepancy = logReadRetryInterval + waitPollInterval + 100*time.Millisecond
	chunkWrites := getWritesForChunks(t, buf)
	for c := 0; c < len(chunkWrites); c++ {
		require.WithinDuration(t, logWriteTimes[chunkWrites[c]], buf.Chunks()[c].Timestamp, maxDiscrepancy,
			"The chunk that captured write %d was chunk %d. The write happened at %s, but the chunk was created at %s, which is not within expected timeframe. Total chunks %d",
			chunkWrites[c],
			c,
			logWriteTimes[chunkWrites[c]].Format(timeWithMilliseconds),
			buf.Chunks()[c].Timestamp.Format(timeWithMilliseconds),
			buf.ChunksLen())
	}
}

// The log watcher may grab more than one write at a time, resulting in fewer "chunks".
// Also the writes can be split between two chunks, so we need to account for that.
// The following assumes that each write writes a number, followed by a newline,
// and that we have at least one valid write in the buffer.
func getWritesForChunks(t *testing.T, buf *testutil.BufferWriter) []int {
	var writes []int
	content := buf.Bytes()

	for _, chunk := range buf.Chunks() {
		var digits []byte

		if content[chunk.Offset] == '\n' {
			// Take the preceding number as the write number--the previous chunk missed the ending newline,
			// so we consider it incomplete.
			for i := chunk.Offset - 1; i >= 0; i-- {
				if content[i] == '\n' {
					break
				}
				digits = append(digits, content[i])
			}
			slices.Reverse(digits)
		} else {
			// We landed in the middle of a number, so we need to find the start and end of the number.
			start := chunk.Offset
			for start >= 0 && content[start] != '\n' {
				start--
			}
			if start < 0 || content[start] == '\n' {
				start++
			}
			end := chunk.Offset
			for content[end] != '\n' {
				end++
			}
			digits = content[start:end]
		}

		write, err := strconv.ParseInt(string(digits), 10, 32)
		require.NoError(t, err)
		writes = append(writes, int(write))
	}

	return writes
}
