package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

// maxMessageBytes bounds one message.
//
// The explicit limit prevents a peer from growing the read buffer without bound.
// It accommodates prompts that carry embedded file contents.
const maxMessageBytes = 64 << 20

// NewStdioTransport returns the agent's side of a local connection: this process's
// own stdin and stdout.
//
// Stdout carries only protocol messages. Send agent diagnostics to stderr.
func NewStdioTransport() Transport {
	return NewIOTransport(os.Stdin, os.Stdout)
}

// NewIOTransport frames newline-delimited JSON over a reader and a writer.
//
// Closing the connection closes both streams so a pending Read can unblock. Wrap
// an uncloseable stream with io.NopCloser when its lifetime is managed elsewhere.
func NewIOTransport(reader io.ReadCloser, writer io.WriteCloser) Transport {
	return &ioTransport{reader: reader, writer: writer}
}

type ioTransport struct {
	singleUse
	reader io.ReadCloser
	writer io.WriteCloser
}

func (t *ioTransport) Connect(ctx context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.reader == nil || t.writer == nil {
		return nil, errors.New("acp: IO transport requires non-nil reader and writer")
	}
	lines := bufio.NewReaderSize(t.reader, 64<<10)
	return &ioConnection{reader: t.reader, writer: t.writer, lines: lines}, nil
}

type ioConnection struct {
	reader io.ReadCloser
	writer io.WriteCloser
	lines  *bufio.Reader

	// writeMu serialises writes, because Write may be called concurrently and one
	// message must not be interleaved with another on the wire.
	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// Read does not consult the context while blocked on the underlying stream: an
// io.Reader cannot be interrupted, and pretending otherwise would return while a
// goroutine stayed blocked in a read that later consumed a message nobody was
// waiting for. Close is what unblocks it.
func (c *ioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			// Blank lines are not messages. Skipping them keeps a stream that
			// somebody pretty-printed into readable rather than fatal.
			continue
		}
		message, err := jsonrpc.DecodeMessage(line)
		if err != nil {
			return nil, fmt.Errorf("acp: reading a message: %w", err)
		}
		return message, nil
	}
}

// readLine applies the limit while assembling a line because bufio may return one
// logical line across many ErrBufferFull reads.
func (c *ioConnection) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := c.lines.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxMessageBytes {
			return nil, fmt.Errorf("acp: message exceeds %d-byte limit", maxMessageBytes)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			// A final message with no trailing newline is still a message.
			return line, nil
		default:
			return nil, err
		}
	}
}

func (c *ioConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("acp: encoding a message: %w", err)
	}
	// One write, not two: a message and its newline reaching the peer separately
	// would be visible to anything reading the stream with a timeout.
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("acp: writing a message: %w", err)
	}
	return nil
}

// Both streams, because unblocking a pending Read is what the Connection contract
// asks of Close and a reader has no other way to be interrupted.
func (c *ioConnection) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = errors.Join(c.reader.Close(), c.writer.Close())
	})
	return c.closeErr
}

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
	// Stderr is left alone, which means it goes wherever the command was
	// configured to send it. A client that wants the agent's diagnostics sets
	// cmd.Stderr before calling this; a client that does not gets the behaviour
	// os/exec already has.
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
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, stdin.Close(), stdout.Close())
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("acp: starting the agent: %w", err),
			stdin.Close(),
			stdout.Close(),
		)
	}

	grace := t.config.TerminationGrace
	if grace == 0 {
		grace = terminationGrace
	}
	return &commandConnection{
		ioConnection: ioConnection{
			reader: stdout,
			writer: stdin,
			lines:  bufio.NewReaderSize(stdout, 64<<10),
		},
		cmd:   cmd,
		grace: grace,
	}, nil
}

type commandConnection struct {
	ioConnection
	cmd   *exec.Cmd
	grace time.Duration

	reapOnce sync.Once
	reapErr  error
}

// Close reaps the agent on a bounded sequence; see reap.
//
// The exit status is not reported. A client that closed the connection expects it
// to be closed, and an agent that exits non-zero, or dies of the signal it was
// sent, has stopped rather than failed. What is reported is a pipe that would not
// close, or a process this package could not reap at all.
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
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// The status is deliberately discarded; see Close.
		_ = c.cmd.Wait()
	}()

	timer := time.NewTimer(c.grace)
	defer timer.Stop()
	waited := func() bool {
		select {
		case <-exited:
			return true
		case <-timer.C:
			timer.Reset(c.grace)
			return false
		}
	}

	// The pipes are already closed, which is the polite way to say stop.
	if waited() {
		return nil
	}
	// Then the platform's way of asking. If asking is not something this platform
	// can do, or it fails, there is no point waiting for an answer.
	if askToStop(c.cmd.Process) && waited() {
		return nil
	}
	// Then the way that is not a request.
	if err := c.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("acp: killing the agent: %w", err)
	}
	if waited() {
		return nil
	}
	return fmt.Errorf("acp: the agent did not exit within %s of being killed", c.grace)
}
