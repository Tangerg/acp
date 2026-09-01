//go:build !windows

package acp

import (
	"os"
	"syscall"
)

// askToStop asks a process to stop, and reports whether it could be asked.
//
// SIGTERM is a request an agent can act on: flush a session, close a terminal, and
// exit. Kill is what follows if it does not, and this is the step that gives it the
// chance.
//
// It reports a bool rather than an error because the caller has one decision to
// make — whether it is worth waiting for an answer — and no use for the reason.
func askToStop(process *os.Process) bool {
	return process.Signal(syscall.SIGTERM) == nil
}
