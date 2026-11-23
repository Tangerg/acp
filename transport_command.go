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

// terminationGrace is how long a subprocess is given to stop at each step of
// being asked, when the caller did not say.
//
// Five seconds gives an agent time to flush while keeping shutdown bounded.
const terminationGrace = 5 * time.Second

// A CommandConfig is how an agent is run as a subprocess.
type CommandConfig struct {
	// Command is the agent, unstarted. Its stdin and stdout become the protocol
	// stream, so setting either is a mistake this reports rather than works
	// around.
	//
	// Stderr is left as configured. Writes to a custom writer must finish because
	// os/exec cannot interrupt user code blocked in Write. A zero WaitDelay uses
	// TerminationGrace to bound draining a pipe inherited by a descendant.
	Command *exec.Cmd

	// TerminationGrace is how long the agent is given to exit at each step of
	// being asked to: once after its stdin is closed, and again after it is
	// signalled. Zero means five seconds.
	//
	// It is here because the right answer is the agent's, not this package's. An
	// agent that persists a session on the way out needs longer than one that does
	// not, and a client with its own shutdown deadline needs shorter.
	TerminationGrace time.Duration
}

// NewCommandTransport returns the client's side of a local connection: it starts
// the agent as a subprocess and frames newline-delimited JSON over its stdin and
// stdout.
//
// Closing the connection closes the pipes and reaps the process within the
// configured shutdown budget.
func NewCommandTransport(config *CommandConfig) Transport {
	var owned CommandConfig
	if config != nil {
		owned = *config
	}
	return &commandTransport{config: owned}
}

type commandTransport struct {
	singleUse
	config CommandConfig
}

func (t *commandTransport) Connect(ctx context.Context) (Connection, error) {
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
		grace = terminationGrace
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
	if startErr := cmd.Start(); startErr != nil {
		return nil, errors.Join(
			fmt.Errorf("acp: starting the agent: %w", startErr),
			stdin.Close(),
			stdout.Close(),
		)
	}

	return &commandConnection{
		ioConnection: newIOConnection(IOConfig{
			Reader: stdout,
			Writer: stdin,
			Close:  func() error { return errors.Join(stdout.Close(), stdin.Close()) },
		}),
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
