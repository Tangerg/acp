package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// defaultTerminationGrace is how long a subprocess is given to stop at each step
// of being asked, when the caller did not say: time to flush a session while
// keeping shutdown bounded.
const defaultTerminationGrace = 5 * time.Second

// A CommandConfig is how an agent is run as a subprocess.
type CommandConfig struct {
	// Command is the agent, unstarted. Its stdin and stdout become the protocol
	// stream, so it must not set either. Stderr is left as configured; a custom
	// writer must finish its own Write calls, which os/exec cannot interrupt. A
	// zero WaitDelay is set to TerminationGrace, which bounds draining a pipe a
	// descendant inherited.
	Command *exec.Cmd

	// TerminationGrace is how long the agent is given to exit at each step of
	// being asked: after its stdin is closed, and again after it is signalled.
	// Zero means five seconds. The right value is the agent's: one that persists
	// a session on the way out needs longer.
	TerminationGrace time.Duration
}

// A CommandTransport is the client's side of a local connection: it starts the
// agent as a subprocess and frames newline-delimited JSON over its stdin and
// stdout. Closing the connection closes the pipes and reaps the process within
// the configured budget.
type CommandTransport struct {
	singleUse
	config CommandConfig
}

// NewCommandTransport returns a transport that runs the configured command. The
// configuration is copied.
func NewCommandTransport(config *CommandConfig) *CommandTransport {
	transport := new(CommandTransport)
	if config != nil {
		transport.config = *config
	}
	return transport
}

// Connect starts the agent. A command runs once, so a second Connect fails.
func (t *CommandTransport) Connect(ctx context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := t.config.Command
	if cmd == nil {
		return nil, errors.New("acp: CommandConfig.Command is required")
	}
	if t.config.TerminationGrace < 0 {
		return nil, errors.New("acp: CommandConfig.TerminationGrace must not be negative")
	}
	grace := t.config.TerminationGrace
	if grace == 0 {
		grace = defaultTerminationGrace
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = grace
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: connecting to the agent's stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("acp: connecting to the agent's stdout: %w", err),
			stdin.Close(),
		)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, stdin.Close(), stdout.Close())
	}
	// A failed Start has already closed the pipes it created.
	if startErr := cmd.Start(); startErr != nil {
		return nil, fmt.Errorf("acp: starting the agent: %w", startErr)
	}

	return &commandConnection{
		ioConnection: newIOConnection(IOConfig{
			Reader: stdout,
			Writer: stdin,
			Close:  func() error { return errors.Join(stdout.Close(), stdin.Close()) },
		}, false),
		cmd:   cmd,
		grace: grace,
	}, nil
}

type commandConnection struct {
	*ioConnection
	cmd   *exec.Cmd
	grace time.Duration

	reapOnce sync.Once
	reapErr  error
}

// The exit status is not reported. A client that closed the connection expects it
// to be closed, and an agent that exits non-zero, or dies of the signal it was
// sent, has stopped rather than failed. WaitDelay may close inherited diagnostic
// pipes; other I/O failures and a failure to reap remain observable.
func (c *commandConnection) Close() error {
	closeErr := c.ioConnection.Close()
	c.reapOnce.Do(func() { c.reapErr = c.reap() })
	return errors.Join(closeErr, c.reapErr)
}

// reap escalates for as long as the process is still there: the pipes are already
// closed, which is the polite way to say stop, then the platform's way of asking,
// then the way that is not a request. Every step is bounded, because ownership of
// a subprocess is not ownership if the owner can be held by it.
func (c *commandConnection) reap() error {
	exited := make(chan error, 1)
	go func() {
		exited <- c.cmd.Wait()
	}()

	waited := func(timeout time.Duration) (bool, error) {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-exited:
			return true, c.waitError(err)
		case <-timer.C:
			return false, nil
		}
	}

	if done, err := waited(c.grace); done {
		return err
	}
	if askToStop(c.cmd.Process) {
		if done, err := waited(c.grace); done {
			return err
		}
	}
	// Wait also joins os/exec's pipe copiers. It being pending does not imply
	// the process is alive, so an already reaped process is not a kill failure.
	killErr := c.cmd.Process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	} else if killErr != nil {
		killErr = fmt.Errorf("acp: killing the agent: %w", killErr)
	}
	if done, err := waited(c.grace); done {
		return errors.Join(killErr, err)
	}
	// The I/O deadline starts after process exit, independently of the process's
	// termination budget. Join it before giving up ownership of the wait task.
	if done, err := waited(c.cmd.WaitDelay); done {
		return errors.Join(killErr, err)
	}
	return errors.Join(killErr, fmt.Errorf(
		"acp: agent shutdown exceeded process grace %s and I/O wait delay %s", c.grace, c.cmd.WaitDelay))
}

func (c *commandConnection) waitError(err error) error {
	var exitError *exec.ExitError
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	// A diagnostic writer can return another subprocess's ExitError. Only the
	// state produced by this command's Wait identifies the managed agent's exit.
	if errors.As(err, &exitError) && exitError != nil &&
		exitError.ProcessState != nil && exitError.ProcessState == c.cmd.ProcessState {
		return nil
	}
	return fmt.Errorf("acp: waiting for the agent: %w", err)
}
