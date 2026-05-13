//go:build !windows

package termpty

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestUnixPtyEndToEnd verifies that the Unix PTY shim can spawn a process,
// stream its output via the master fd, and report its exit code.
// This is a targeted smoke test for the macOS/Linux implementation - it
// runs against the real /dev/ptmx so the build host must support PTYs
// (every supported macOS/Linux dev/CI machine does).
func TestUnixPtyEndToEnd(t *testing.T) {
	tp, err := StartProcess(context.Background(), CommandSpec{
		Cmd:  []string{"/bin/sh", "-c", "echo hello-from-pty; sleep 0.05; exit 7"},
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// Drain the PTY output until the child closes its end (slave EOF).
	// Cap the read so the test cannot hang if something goes wrong.
	done := make(chan struct{})
	var got []byte
	go func() {
		defer close(done)
		got, _ = io.ReadAll(tp.PTY)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = tp.PTY.Close()
		t.Fatal("timed out waiting for PTY EOF")
	}

	if !strings.Contains(string(got), "hello-from-pty") {
		t.Errorf("PTY output missing expected text; got %q", got)
	}

	exit := tp.WaitExit(context.Background())
	if exit != 7 {
		t.Errorf("expected exit code 7, got %d", exit)
	}

	if tp.PID <= 0 {
		t.Errorf("expected positive PID, got %d", tp.PID)
	}
	if tp.IdentityTime.IsZero() {
		t.Errorf("expected non-zero IdentityTime")
	}
}

// TestUnixPtyResize verifies Resize doesn't error for a started PTY.
func TestUnixPtyResize(t *testing.T) {
	tp, err := StartProcess(context.Background(), CommandSpec{
		Cmd:  []string{"/bin/sh", "-c", "sleep 0.5"},
		Cols: 80,
		Rows: 24,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer tp.PTY.Close()

	if err := tp.PTY.Resize(132, 50); err != nil {
		t.Errorf("Resize: %v", err)
	}
	if err := tp.PTY.Resize(0, 50); err == nil {
		t.Error("expected Resize(0,50) to error")
	}

	_ = tp.WaitExit(context.Background())
}
