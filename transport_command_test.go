package acp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/Tangerg/acp"
)

func TestCommandShutdownJoinsInheritedDiagnosticPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX process inheritance")
	}
	for _, waitDelay := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run(waitDelay.String(), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 0.5 >&2 & exit 0")
			cmd.Stderr = new(bytes.Buffer)
			cmd.WaitDelay = waitDelay
			const grace = 20 * time.Millisecond
			stream, err := acp.NewCommandTransport(&acp.CommandConfig{
				Command: cmd, TerminationGrace: grace,
			}).Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stream.Close() })
			if _, err := stream.Read(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("waiting for the parent's stdout to close: %v", err)
			}
			if waitDelay != 0 && cmd.WaitDelay != waitDelay {
				t.Errorf("configured WaitDelay changed from %s to %s", waitDelay, cmd.WaitDelay)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("cleanly exited process reported shutdown failure: %v", err)
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatal("Close returned before reaping the process")
			}
		})
	}
}

func TestCommandShutdownReportsDiagnosticWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture writes diagnostics through a POSIX shell")
	}
	for _, want := range []error{errors.New("diagnostic sink failed"), context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf diagnostic >&2")
			cmd.Stderr = failingDiagnosticWriter{err: want}
			stream, err := acp.NewCommandTransport(&acp.CommandConfig{Command: cmd}).Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stream.Close() })
			if err := stream.Close(); !errors.Is(err, want) {
				t.Fatalf("Close = %v, want diagnostic failure %v", err, want)
			}
		})
	}
}

type failingDiagnosticWriter struct{ err error }

func (w failingDiagnosticWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCommandShutdownPreservesDiagnosticPipelineExitFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX test fixture")
	}
	pipelineErr := exec.CommandContext(t.Context(), "sh", "-c", "exit 7").Run()
	if pipelineErr == nil {
		t.Fatal("fixture process unexpectedly succeeded")
	}
	want := fmt.Errorf("diagnostic pipeline failed: %w", pipelineErr)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf diagnostic >&2; exit 0")
	cmd.Stderr = failingDiagnosticWriter{err: want}
	stream, err := acp.NewCommandTransport(&acp.CommandConfig{Command: cmd, TerminationGrace: time.Second}).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := stream.Close()
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("agent did not exit successfully: %v", cmd.ProcessState)
	}
	if !errors.Is(got, want) {
		t.Errorf("successful agent lost its stderr sink failure: Close=%v, want %v", got, want)
	}
}
