// Copyright (c) Microsoft Corporation. All rights reserved.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/lockfile"
)

func main() {
	err := mainCommand().Execute()
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
}

type lfWriterFlags struct {
	runDuration   time.Duration
	instanceName  string
	writeInterval time.Duration
	truncateAfter int
}

var (
	flags lfWriterFlags
)

func mainCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lfwriter <path to lock file>",
		Short: "Test program that repeatedly writes incrementing numbers to a lockfile",
		RunE:  executeMain,
		Args:  cobra.ExactArgs(1),
	}

	cmd.Flags().DurationVarP(&flags.runDuration, "run-duration", "d", 2*time.Second, "The amount of time to run the program for")
	cmd.Flags().StringVarP(&flags.instanceName, "instance-name", "i", "default", "The name of the instance")
	cmd.Flags().DurationVarP(&flags.writeInterval, "write-interval", "w", 50*time.Millisecond, "The interval between write attempts")
	cmd.Flags().IntVarP(&flags.truncateAfter, "truncate-after", "t", 0, "Truncate the file after this many writes")

	return cmd
}

func executeMain(cmd *cobra.Command, args []string) error {
	path := args[0]
	if len(path) == 0 {
		return fmt.Errorf("path to lock file must be provided")
	}

	lf, err := lockfile.NewLockfile(path)
	if err != nil {
		return fmt.Errorf("failed to create lockfile: %w", err)
	}
	defer func() {
		_ = lf.Close()
		// Do not remove--there might be multiple instances working on the same file
	}()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(flags.runDuration))
	defer cancel()

	writeInterval := wait.Jitter(flags.writeInterval, 0.1)
	updateCount := 0

	for ctx.Err() == nil {
		if flags.truncateAfter > 0 && updateCount >= flags.truncateAfter {
			updateCount = 0

			// Need to lock the file to truncate it
			lockErr := lf.TryLock(ctx, lockfile.DefaultLockRetryInterval)
			if lockErr == context.DeadlineExceeded {
				break // the program ran its course
			} else if lockErr != nil {
				return fmt.Errorf("failed to lock file: %w", lockErr)
			}

			truncateErr := lf.Truncate(0)
			if truncateErr != nil {
				return fmt.Errorf("failed to truncate file: %w", truncateErr)
			}

			// No need to unlock because we are about to write into the file
		}

		updateErr := updateFile(ctx, lf)
		if updateErr != nil {
			return updateErr
		}

		updateCount++
		time.Sleep(writeInterval)
	}

	if updateCount == 0 {
		return fmt.Errorf("no updates were made to the file")
	}

	return nil
}

func updateFile(ctx context.Context, lf *lockfile.Lockfile) error {
	lockErr := lf.TryLock(ctx, lockfile.DefaultLockRetryInterval)
	if lockErr == context.DeadlineExceeded {
		return nil // the program ran its course
	} else if lockErr != nil {
		return fmt.Errorf("failed to lock file: %w", lockErr)
	}

	defer func() {
		unlockErr := lf.Unlock()
		if unlockErr != nil {
			panic(fmt.Errorf("failed to unlock file: %w", unlockErr))
		}
	}()

	_, seekErr := lf.Seek(0, io.SeekStart)
	if seekErr != nil {
		return fmt.Errorf("failed to seek to start of file: %w", seekErr)
	}

	num, readErr := getLastNumber(lf)
	if readErr != nil {
		return fmt.Errorf("failed to read last number: %w", readErr)
	}

	num++
	_, writeErr := fmt.Fprintf(lf, "%d %s\n", num, flags.instanceName)
	if writeErr != nil {
		return fmt.Errorf("failed to write number: %w", writeErr)
	}

	return nil
}

func getLastNumber(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(bufio.NewReader(r))
	retval := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		parts := strings.SplitN(string(line), " ", 2)
		num, parseErr := strconv.ParseInt(parts[0], 10, 32)
		if parseErr != nil {
			return -1, fmt.Errorf("failed to parse number: %w", parseErr)
		}
		if int(num) > retval {
			retval = int(num)
		}
	}

	return retval, nil
}
