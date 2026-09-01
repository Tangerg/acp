//go:build windows

package acp

import "os"

// askToStop reports that there is no way to ask on this platform.
//
// Windows has no signal a parent can send a child console process to mean "stop
// when you are ready". Signal accepts only os.Kill, and the graceful equivalents —
// GenerateConsoleCtrlEvent, or a job object with kill-on-close — need the child to
// have been created into a console group or a job, which is a decision about how
// the command is started and therefore the caller's rather than this package's.
//
// Saying so is better than pretending: the caller goes straight to killing the
// process rather than waiting out a grace period for a request nobody sent.
func askToStop(*os.Process) bool {
	return false
}
