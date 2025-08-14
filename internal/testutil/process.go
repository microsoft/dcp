package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/usvc-apiserver/pkg/process"
)

func EnsureProcessTree(t *testing.T, rootP process.ProcessTreeItem, expectedSize int, timeout time.Duration) {
	processesStartedCtx, processesStartedCancelFn := context.WithTimeout(context.Background(), timeout)
	defer processesStartedCancelFn()

	var processTreeLen int
	err := wait.PollUntilContextCancel(
		processesStartedCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			processTree, err := process.GetProcessTree(rootP)
			if err != nil {
				return false, err
			}
			processTreeLen = len(processTree)
			return processTreeLen == expectedSize, nil
		},
	)

	require.NoError(t, err, "expected number of 'delay' program instances not found (expected %d, actual %d)", expectedSize, processTreeLen)
}
