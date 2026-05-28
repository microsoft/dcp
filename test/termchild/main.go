/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// termchild is a cross-platform helper executable used by the internal/termpty package tests.
// It is launched attached to a pseudo-terminal and exercises terminal I/O scenarios
// that the tests assert against (stdout/stderr output, stdin echo, terminal resize,
// graceful and ignored termination signals, hang-up detection).
//
// The behavior of a single invocation is composed via independent flags
// so a test can describe exactly what the child should do without needing platform-specific shell scripts.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/microsoft/dcp/pkg/logger"
)

func main() {
	log := logger.New("termchild")
	rootCmd := newMainCommand(log.Logger)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "termchild: %v\n", err)
		os.Exit(2)
	}
	os.Exit(int(finalExitCode.Load()))
}

type termchildFlagData struct {
	printStdout        string
	printStderr        string
	disableEcho        bool
	echo               bool
	ignoreSigterm      bool
	hupExitCode        int
	hupExitCodeSet     bool
	sigtermExitCode    int
	sigtermExitCodeSet bool
	resizeExitCode     int
	resizeExitSet      bool
	reportSize         bool
	wait               bool
	exitCode           int
	waitMaxDuration    time.Duration
}

var (
	flags termchildFlagData

	// finalExitCode is the exit code reported to the OS. We use an atomic so
	// that signal/resize/hup handler goroutines can override it without
	// racing with main goroutine completion. main() reads this value once,
	// after Cobra returns.
	finalExitCode atomic.Int32
)

const (
	printFlag           = "print"
	printStderrFlag     = "print-stderr"
	disableEchoFlag     = "disable-echo"
	echoFlag            = "echo"
	ignoreSigtermFlag   = "ignore-sigterm"
	hupExitCodeFlag     = "hup-exit-code"
	sigtermExitCodeFlag = "sigterm-exit-code"
	resizeExitCodeFlag  = "resize-exit-code"
	reportSizeFlag      = "report-size"
	waitFlag            = "wait"
	exitCodeFlag        = "exit-code"
	waitMaxDurationFlag = "wait-max-duration"
)

func newMainCommand(log logr.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "termchild",
		Short: "Cross-platform PTY test child program used by internal/termpty tests.",
		Long: `termchild performs composable terminal I/O actions against the controlling
pseudo-terminal. Flags are independent; specify any combination needed by
the test. Execution order:

  1. Apply --disable-echo (where supported).
  2. If --report-size, print "init-size:COLSxROWS".
  3. Install signal handlers: --ignore-sigterm, --hup-exit-code, --sigterm-exit-code, --resize-exit-code (these run for the rest of the process lifetime).
  4. Print --print to stdout and --print-stderr to stderr.
  5. If --echo, read lines from stdin and write "got:LINE" until EOF.
  6. If --wait, block until cancelled or a non-ignored signal arrives.
  7. Exit with --exit-code (default 0). Signal/hup/resize handlers can override the exit code before the process exits.`,
		RunE: func(c *cobra.Command, _ []string) error {
			flags.hupExitCodeSet = c.Flags().Changed(hupExitCodeFlag)
			flags.sigtermExitCodeSet = c.Flags().Changed(sigtermExitCodeFlag)
			flags.resizeExitSet = c.Flags().Changed(resizeExitCodeFlag)
			return runMain(log)
		},
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.Flags().StringVar(&flags.printStdout, printFlag, "",
		"Text to print to stdout (newline-terminated). Empty means do not print.")
	cmd.Flags().StringVar(&flags.printStderr, printStderrFlag, "",
		"Text to print to stderr (newline-terminated). Empty means do not print.")
	cmd.Flags().BoolVar(&flags.disableEcho, disableEchoFlag, false,
		"Disable terminal input echo (and on Windows, also line-input mode) before any I/O.")
	cmd.Flags().BoolVar(&flags.echo, echoFlag, false,
		`Read lines from stdin and write "got:LINE" to stdout until EOF.`)
	cmd.Flags().BoolVar(&flags.ignoreSigterm, ignoreSigtermFlag, false,
		"Install a handler that ignores SIGTERM (Unix) or CTRL_BREAK/CTRL_C (Windows).")
	cmd.Flags().IntVar(&flags.hupExitCode, hupExitCodeFlag, 0,
		"Exit code to use when SIGHUP (Unix) or CTRL_CLOSE (Windows) is received.")
	cmd.Flags().IntVar(&flags.sigtermExitCode, sigtermExitCodeFlag, 0,
		"Exit code to use when SIGTERM/SIGINT/SIGQUIT (Unix) or CTRL_C (Windows) is received. Has no effect if --ignore-sigterm is set.")
	cmd.Flags().IntVar(&flags.resizeExitCode, resizeExitCodeFlag, 0,
		`Exit code to use after the first terminal-size change. Also prints "size:COLSxROWS" first.`)
	cmd.Flags().BoolVar(&flags.reportSize, reportSizeFlag, false,
		`Print "init-size:COLSxROWS" before any other action.`)
	cmd.Flags().BoolVar(&flags.wait, waitFlag, false,
		"Block after preceding actions until cancelled or a non-ignored signal arrives.")
	cmd.Flags().IntVar(&flags.exitCode, exitCodeFlag, 0,
		"Final exit code if no override fires.")
	cmd.Flags().DurationVar(&flags.waitMaxDuration, waitMaxDurationFlag, 5*time.Minute,
		"Safety cap for --wait so a stuck test doesn't leave the process running forever.")

	return cmd
}

func runMain(log logr.Logger) error {
	finalExitCode.Store(int32(flags.exitCode))

	if flags.disableEcho {
		if err := disableTerminalEcho(); err != nil {
			return fmt.Errorf("disable-echo: %w", err)
		}
	}

	if flags.reportSize {
		cols, rows, err := getTerminalSize()
		if err != nil {
			return fmt.Errorf("report-size: %w", err)
		}
		if _, writeErr := fmt.Fprintf(os.Stdout, "init-size:%dx%d\n", cols, rows); writeErr != nil {
			return fmt.Errorf("report-size: write: %w", writeErr)
		}
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	installSignalDispatcher(log, shutdownCancel)

	if flags.resizeExitSet {
		if err := installResizeHandler(log, flags.resizeExitCode, shutdownCancel); err != nil {
			return fmt.Errorf("resize-exit-code: %w", err)
		}
	}

	if flags.printStdout != "" {
		if _, err := fmt.Fprintln(os.Stdout, flags.printStdout); err != nil {
			return fmt.Errorf("print: %w", err)
		}
	}
	if flags.printStderr != "" {
		if _, err := fmt.Fprintln(os.Stderr, flags.printStderr); err != nil {
			return fmt.Errorf("print-stderr: %w", err)
		}
	}

	if flags.echo {
		if err := runEchoLoop(shutdownCtx); err != nil {
			return fmt.Errorf("echo: %w", err)
		}
	}

	if flags.wait {
		waitCtx, waitCancel := context.WithTimeout(shutdownCtx, flags.waitMaxDuration)
		defer waitCancel()
		<-waitCtx.Done()
		log.V(1).Info("Wait ended", "reason", waitCtx.Err())
	}

	return nil
}

// runEchoLoop reads stdin line by line and writes "got:LINE" to stdout.
// Returns nil on EOF or context cancellation, and an error on a read failure that is not EOF.
func runEchoLoop(ctx context.Context) error {
	reader := bufio.NewReader(os.Stdin)

	type lineResult struct {
		line []byte
		err  error
	}

	for {
		ch := make(chan lineResult, 1)

		go func() {
			line, err := reader.ReadBytes('\n')
			ch <- lineResult{line: line, err: err}
		}()

		select {
		case <-ctx.Done():
			return nil

		case res := <-ch:
			if len(res.line) > 0 {
				// Strip trailing newline (and CR for the ConPTY case where Enter arrives as \r\n).
				trimmed := stripLineEnding(res.line)
				if _, werr := fmt.Fprintf(os.Stdout, "got:%s\n", trimmed); werr != nil {
					return werr
				}
			}

			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return nil
				}
				return res.err
			}
		}
	}
}

func stripLineEnding(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return string(b[:end])
}

type signalKind int

const (
	signalUnknown signalKind = iota
	signalShutdown
	signalHup
)

// installSignalDispatcher wires all relevant OS signals to a single
// goroutine that decides whether they should be ignored (--ignore-sigterm),
// trigger the hup exit-code override (--hup-exit-code), or just cancel the
// shutdown context. Centralizing the decision avoids races between multiple
// handlers that would otherwise contend over finalExitCode and cancel().
//
// Signal classification is platform-specific: see classifySignal in the termchild_unix.go / termchild_windows.go file.

func installSignalDispatcher(log logr.Logger, cancel context.CancelFunc) {
	ch := make(chan os.Signal, 4)
	registerSignals(ch)

	go func() {
		defer signal.Stop(ch)

		for sig := range ch {
			kind := classifySignal(sig)
			log.V(1).Info("Received signal", "signal", sig.String(), "kind", kind)

			switch kind {
			case signalHup:
				if flags.ignoreSigterm {
					log.V(1).Info("Hup-like signal ignored due to --ignore-sigterm", "signal", sig.String())
					continue
				}

				log.V(1).Info("Hup-like signal treated as shutdown", "signal", sig.String())

				if flags.hupExitCodeSet {
					finalExitCode.Store(int32(flags.hupExitCode))
				}

			case signalShutdown:
				if flags.ignoreSigterm {
					log.V(1).Info("Shutdown signal ignored due to --ignore-sigterm", "signal", sig.String())
					continue
				}

				log.V(1).Info("Shutdown signal received, exiting...", "signal", sig.String())

				if flags.sigtermExitCodeSet {
					finalExitCode.Store(int32(flags.sigtermExitCode))
				}

			default:
				log.V(1).Info("Unknown signal kind; treating as shutdown", "signal", sig.String())
			}

			cancel()
			return
		}
	}()
}
