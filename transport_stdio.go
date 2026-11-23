package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unicode/utf8"

	"github.com/Tangerg/acp/jsonrpc"
)

// maxMessageBytes bounds one message.
//
// The explicit limit prevents a peer from growing the read buffer without bound.
// It accommodates prompts that carry embedded file contents.
const maxMessageBytes = 64 << 20

// readBufferBytes is where a line starts, not where it ends: readLine keeps
// reading past a full buffer, so this trades a larger first read against the
// memory a connection holds while idle.
const readBufferBytes = 64 << 10

// NewStdioTransport returns the agent's side of a local connection: this process's
// own stdin and stdout.
//
// Stdout carries only protocol messages. Send agent diagnostics to stderr.
func NewStdioTransport() Transport {
	reader, writer := os.Stdin, os.Stdout
	return NewIOTransport(&IOConfig{
		Reader: reader,
		Writer: writer,
		Close:  func() error { return errors.Join(reader.Close(), writer.Close()) },
	})
}

// IOConfig separates the byte streams from the owner that releases them.
// All fields are required.
type IOConfig struct {
	Reader io.Reader
	Writer io.Writer

	// Close releases the resources behind both streams and must unblock pending
	// reads and writes. The connection invokes it once on shutdown.
	// For one shared resource, such as a net.Conn, use that resource's Close.
	// For separate resources, close both and report their errors.
	Close func() error
}

// NewIOTransport frames newline-delimited JSON over the configured byte streams.
//
// The configuration is copied. Closing the connection or interrupting a write
// invokes its Close callback; the reader and writer do not own release. A failed
// Connect leaves the resources with the caller.
func NewIOTransport(config *IOConfig) Transport {
	var owned IOConfig
	if config != nil {
		owned = *config
	}
	return &ioTransport{config: owned}
}

type ioTransport struct {
	singleUse
	config IOConfig
}

func (t *ioTransport) Connect(ctx context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.config.Reader == nil || t.config.Writer == nil || t.config.Close == nil {
		return nil, errors.New("acp: IOConfig requires Reader, Writer, and Close")
	}
	return newIOConnection(t.config), nil
}

type ioConnection struct {
	writer  io.Writer
	lines   *bufio.Reader
	release func() error

	// A waiting writer must be cancellable before it owns the byte stream.
	writing chan struct{}

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

func newIOConnection(config IOConfig) *ioConnection {
	return &ioConnection{
		writer:  config.Writer,
		lines:   bufio.NewReaderSize(config.Reader, readBufferBytes),
		release: config.Close,
		writing: make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
}

// Read does not consult the context while blocked on the underlying stream: an
// io.Reader cannot be interrupted, and pretending otherwise would return while a
// goroutine stayed blocked in a read that later consumed a message nobody was
// waiting for. Close is what unblocks it.
func (c *ioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-c.closed:
		return nil, ErrConnectionClosed
	default:
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(line) {
			return nil, errors.New("acp: message is not valid UTF-8")
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

	select {
	case c.writing <- struct{}{}:
		defer func() { <-c.writing }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrConnectionClosed
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	select {
	case <-c.closed:
		return ErrConnectionClosed
	default:
	}

	// Once a byte write starts, cancellation cannot prove which prefix reached
	// the peer. Close the owned streams and join the callback before returning;
	// no later write may race an unfinished interruption of this one.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.Close()
		close(interrupted)
	})
	written, writeErr := c.writer.Write(data)
	if !stop() {
		<-interrupted
		return fmt.Errorf("acp: interrupted a message write: %w", errors.Join(ctx.Err(), writeErr))
	}
	if writeErr != nil {
		return fmt.Errorf("acp: writing a message: %w", writeErr)
	}
	if written != len(data) {
		return fmt.Errorf("acp: writing a message: %w", io.ErrShortWrite)
	}
	return nil
}

// The release callback is the only owner of the underlying resources, even when
// the reader and writer are two views of one stream.
func (c *ioConnection) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = c.release()
	})
	return c.closeErr
}
