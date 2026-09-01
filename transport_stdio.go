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
// A peer that sends a gigabyte on one line is either broken or hostile, and
// bufio's own answer to a line that does not fit is an error the reader cannot
// recover from — so the limit is stated rather than inherited. It is generous:
// the largest ordinary message is a prompt carrying embedded file contents.
const maxMessageBytes = 64 << 20

// NewStdioTransport returns the agent's side of a local connection: this process's
// own stdin and stdout.
//
// An agent's stdout is the protocol stream. One fmt.Println corrupts it, and the
// failure surfaces at the other end as an unrelated parse error — so this names
// the streams it uses rather than reaching for the globals from inside, which is
// what makes the collision visible in the code that caused it. An agent that wants
// to print should print to stderr, which the client conventionally forwards to its
// log.
func NewStdioTransport() Transport {
	return NewIOTransport(os.Stdin, os.Stdout)
}

// NewIOTransport frames newline-delimited JSON over a reader and a writer.
//
// Both are closed when the connection is: a reader with no way to close it leaves
// a pending Read, and the goroutine blocked in it, with no way to stop. That is
// why this takes the streams rather than a bare io.Reader and io.Writer — a
// caller who genuinely has unclosable streams can wrap them in io.NopCloser and
// has then said so.
func NewIOTransport(reader io.ReadCloser, writer io.WriteCloser) Transport {
	return &ioTransport{reader: reader, writer: writer}
}

type ioTransport struct {
	reader io.ReadCloser
	writer io.WriteCloser

	once      sync.Once
	connected bool
}

func (t *ioTransport) Connect(context.Context) (Connection, error) {
	t.once.Do(func() { t.connected = true })
	if !t.connected {
		return nil, errors.New("acp: this transport is already connected")
	}
	t.connected = false

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

// Read returns the next message.
//
// The context is not consulted while blocked on the underlying stream: an
// io.Reader has no way to be interrupted, and pretending otherwise would return
// while a goroutine stayed blocked in a read that later consumed a message
// nobody was waiting for. Close is what unblocks a Read, which is what the
// Connection contract asks for.
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

// readLine reads one newline-delimited message, refusing one that is absurdly
// long rather than growing a buffer until the process dies.
func (c *ioConnection) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := c.lines.ReadSlice('\n')
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(line) > maxMessageBytes {
				return nil, fmt.Errorf("acp: a message exceeded %d bytes", maxMessageBytes)
			}
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

// Close closes both streams, which is what unblocks a pending Read. It is
// idempotent, and reports the first failure rather than the last.
func (c *ioConnection) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = errors.Join(c.reader.Close(), c.writer.Close())
	})
	return c.closeErr
}

// terminationGrace is how long a subprocess is given to stop at each step of
// being asked, when the caller did not say.
//
// Five seconds is the same budget the MCP Go SDK gives, and the same order as the
// one this package gives a peer to be told about a cancellation. It is long enough
// for an agent to flush what it was writing and short enough that a client
// shutting down is not left wondering.
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
// Closing the connection closes the pipes and reaps the process on a bounded
// sequence, so that a client that stops talking to an agent neither leaves one
// running nor waits on one for ever.
func NewCommandTransport(config *CommandConfig) Transport {
	return &commandTransport{config: *config}
}

type commandTransport struct {
	config CommandConfig

	once      sync.Once
	connected bool
}

func (t *commandTransport) Connect(context.Context) (Connection, error) {
	t.once.Do(func() { t.connected = true })
	if !t.connected {
		return nil, errors.New("acp: this transport is already connected")
	}
	t.connected = false

	cmd := t.config.Command
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: connecting to the agent's stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: connecting to the agent's stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: starting the agent: %w", err)
	}

	grace := t.config.TerminationGrace
	if grace <= 0 {
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

// Close closes the pipes and reaps the process, on a bounded sequence.
//
// Closing the pipes is what releases the read loop, and it is what an agent is
// meant to read as "no more requests are coming". An agent that takes that hint
// exits, and the first wait reaps it.
//
// An agent that does not is asked, and then made, to stop. Every step has a
// deadline, because ownership of a subprocess is not ownership if the owner can be
// held by it: an agent that ignores end-of-input and keeps a background thread
// alive used to block this call, and with it the client's Close, for ever.
//
// The process's exit status is not the connection's error. A client that closed
// the connection expects it to be closed; an agent that exits non-zero, or dies of
// the signal it was sent, has not failed — it has stopped. What is reported is a
// failure to close a pipe, or a process this package could not reap at all.
func (c *commandConnection) Close() error {
	closeErr := c.ioConnection.Close()
	c.reapOnce.Do(func() { c.reapErr = c.reap() })
	return errors.Join(closeErr, c.reapErr)
}

// reap waits for the process, escalating as long as it is still there.
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
