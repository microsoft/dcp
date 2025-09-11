package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

func main() {
	log := logger.New("delay")
	err := newMainCommand(log).Execute()
	if err != nil {
		os.Exit(1)
	} else {
		os.Exit(flags.exitCode)
	}
}

type delayFlagData struct {
	delay         time.Duration
	exitCode      int
	childSpec     string
	ignoreSigTerm bool
}

var (
	flags delayFlagData
)

const (
	delayFlag         = "delay"
	exitCodeFlag      = "exit-code"
	childSpecFlag     = "child-spec"
	ignoreSigtermFlag = "ignore-sigterm"
)

func newMainCommand(log *logger.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delay",
		Short: "A simple program that waits for a specified number of time before exiting.",
		Long: `A simple program that waits for a specified number of time before exiting.

It will exit sooner if SIGTERM or SIGINT is received.
You can also ask it to exit with a specific exit code.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMain(log.Logger)
		},
		Args: cobra.NoArgs,
	}

	cmd.Flags().DurationVarP(&flags.delay, delayFlag, "d", 0, "The amount of time to wait before exiting, for example 3s (3 seconds), 1h10m15s (one hour, ten minutes ,and 15 seconds). Use ms suffix for milliseconds, us suffix for microseconds.")
	cmd.Flags().IntVarP(&flags.exitCode, exitCodeFlag, "e", 0, "The exit code to return when the program exits. If omitted, the program will exit with code 0.")
	cmd.Flags().StringVar(&flags.childSpec, childSpecFlag, "", "How many child, grandchild, etc. processes to run. For example, the value '2,1' will result in running 2 child processes, each of which will run 1 child on their own (two children, and two grandchildren total). The children will use the same values for delay and exit code as the parent process. The parent process will NOT pass any signals to the children, so if signals are used to stop the program, they have to be sent to each descendant separately.")
	cmd.Flags().BoolVar(&flags.ignoreSigTerm, ignoreSigtermFlag, false, "If specified, the program will ignore SIGTERM signal. SIGINT will still work as an early exit request.")

	return cmd
}

func runMain(log logr.Logger) error {
	if flags.exitCode < 0 || flags.exitCode > 125 {
		return fmt.Errorf("the exit code must be between 0 and 125 inclusive")
	}

	err := runChildrenAsNeeded()
	if err != nil {
		return err
	}

	shutdownCh := make(chan os.Signal, 1)
	ctx, shutdownCancelFn := context.WithCancel(context.Background())
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for {
			sig := <-shutdownCh
			log.Info("Received signal", "signal", sig)
			if flags.ignoreSigTerm && (sig == syscall.SIGTERM || sig == os.Interrupt) {
				continue
			} else {
				break
			}
		}
		shutdownCancelFn()
	}()

	if flags.delay != 0 {
		var cancelFn context.CancelFunc
		ctx, cancelFn = context.WithTimeout(ctx, flags.delay)
		defer cancelFn()
	}

	<-ctx.Done()

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		log.Info("Received request to exit")
		fmt.Fprintln(os.Stdout, "Received request to exit")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		log.Info("Ran to completion")
		fmt.Fprintln(os.Stdout, "Ran to completion")
	default:
		log.Error(ctx.Err(), "Unexpected context copletion")
		fmt.Fprintf(os.Stdout, "Unexpected context completion: %v\n", ctx.Err())
	}

	if flags.exitCode != 0 {
		os.Exit(flags.exitCode)
	}

	return nil
}

func runChildrenAsNeeded() error {
	if flags.childSpec == "" {
		return nil
	}

	descendantCounts := strings.Split(flags.childSpec, ",")
	childCount, err := strconv.ParseUint(descendantCounts[0], 10, 8) // Convert to uint8, base 10
	if err != nil {
		return fmt.Errorf("invalid child spec: %w", err)
	}
	if childCount == 0 {
		return fmt.Errorf("invalid child spec: number of children cannot be zero")
	}

	delayExec, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not get the path of the delay program: %w", err)
	}
	childExecArgs := []string{}
	if flags.delay != 0 {
		childExecArgs = append(childExecArgs, fmt.Sprintf("--%s", delayFlag), flags.delay.String())
	}
	if flags.exitCode != 0 {
		childExecArgs = append(childExecArgs, fmt.Sprintf("--%s", exitCodeFlag), strconv.Itoa(flags.exitCode))
	}
	descendantCounts = descendantCounts[1:]
	if len(descendantCounts) > 0 {
		childExecArgs = append(childExecArgs, fmt.Sprintf("--%s", childSpecFlag), strings.Join(descendantCounts, ","))
	}

	for i := uint64(0); i < childCount; i++ {
		cmd := exec.Command(delayExec, childExecArgs...)

		process.DecoupleFromParent(cmd)

		err = cmd.Start()
		if err != nil {
			return fmt.Errorf("could not start a child process: %w", err)
		}

		// If the child exits first, make sure we wait for it so it doesn't become a zombie.
		go func(cmd *exec.Cmd) {
			_ = cmd.Wait()
		}(cmd)
	}

	return nil
}
