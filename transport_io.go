package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// maxMessageBytes bounds one message, so a peer cannot grow the read buffer
// without bound. It accommodates prompts that embed file contents.
const maxMessageBytes = 64 << 20

// readBufferBytes is where a line starts, not where it ends: a longer line keeps
// reading past a full buffer.
const readBufferBytes = 64 << 10

// IOConfig separates the byte streams from the owner that releases them. All
// fields are required.
type IOConfig struct {
	Reader io.Reader
	Writer io.Writer

	// Close releases the resources behind both streams and must make pending
	// reads and writes return. The connection calls it once. For one shared
	// resource, such as a net.Conn, use that resource's Close; for separate ones,
	// close both and report their errors.
	Close func() error
}

// An IOTransport frames newline-delimited JSON-RPC over byte streams.
type IOTransport struct {
	singleUse
	config IOConfig
}

// NewIOTransport returns a transport over the configured streams. The
// configuration is copied; a failed Connect leaves the resources with the
// caller.
func NewIOTransport(config *IOConfig) *IOTransport {
	transport := new(IOTransport)
	if config != nil {
		transport.config = *config
	}
	return transport
}

// Connect hands the streams to the connection, whose Close releases them. A
// transport connects once.
func (t *IOTransport) Connect(ctx context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.config.Reader == nil || t.config.Writer == nil || t.config.Close == nil {
		return nil, errors.New("acp: IOConfig requires Reader, Writer, and Close")
	}
	return newIOConnection(t.config, false), nil
}

// An ioConnection frames messages over a reader and a writer.
//
// When its streams are interruptible, Close makes blocked reads and writes
// return by releasing the streams. When they are not — a process's inherited
// standard streams, which closing does not interrupt — each blocking call runs on
// a goroutine of its own that Read and Write abandon at Close, leaving it blocked
// until the process exits or the stream next moves.
type ioConnection struct {
	writer        io.Writer
	lines         *bufio.Reader
	release       func() error
	uninterrupted bool

	// writing is the write slot: a waiting writer can still give up before it
	// owns the stream.
	writing chan struct{}

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

func newIOConnection(config IOConfig, uninterrupted bool) *ioConnection {
	return &ioConnection{
		writer:        config.Writer,
		lines:         bufio.NewReaderSize(config.Reader, readBufferBytes),
		release:       config.Close,
		uninterrupted: uninterrupted,
		writing:       make(chan struct{}, 1),
		closed:        make(chan struct{}),
	}
}

// Read does not consult its context while blocked: an io.Reader cannot be
// interrupted, and Close is what ends a pending Read.
func (c *ioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		line, err := c.nextLine()
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			// Blank lines are not messages, and skipping them keeps a stream that
			// somebody pretty-printed into readable.
			continue
		}
		return jsonrpc.DecodeMessage(line)
	}
}

func (c *ioConnection) nextLine() ([]byte, error) {
	select {
	case <-c.closed:
		return nil, ErrConnectionClosed
	default:
	}
	if !c.uninterrupted {
		return c.readLine()
	}
	type result struct {
		line []byte
		err  error
	}
	read := make(chan result, 1)
	go func() {
		line, err := c.readLine()
		read <- result{line, err}
	}()
	select {
	case got := <-read:
		return got.line, got.err
	case <-c.closed:
		return nil, ErrConnectionClosed
	}
}

// readLine applies the size limit while assembling a line, because bufio may
// return one logical line across many ErrBufferFull reads.
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

// Write honours its context until it owns the stream. From the first byte it
// finishes or fails, because cancellation cannot prove which prefix reached the
// peer.
func (c *ioConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("acp: encoding a message: %w", err)
	}
	// One write, not two: a message and its newline reaching the peer separately
	// would be visible to a reader with a timeout.
	data = append(data, '\n')

	select {
	case c.writing <- struct{}{}:
		defer func() { <-c.writing }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrConnectionClosed
	}
	select {
	case <-c.closed:
		return ErrConnectionClosed
	default:
	}
	return c.writeAll(data)
}

func (c *ioConnection) writeAll(data []byte) error {
	if !c.uninterrupted {
		return c.writeOnce(data)
	}
	written := make(chan error, 1)
	go func() { written <- c.writeOnce(data) }()
	select {
	case err := <-written:
		return err
	case <-c.closed:
		return ErrConnectionClosed
	}
}

// writeOnce treats a short write as a failure rather than a message.
func (c *ioConnection) writeOnce(data []byte) error {
	n, err := c.writer.Write(data)
	switch {
	case err != nil:
		return fmt.Errorf("acp: writing a message: %w", err)
	case n != len(data):
		return fmt.Errorf("acp: writing a message: %w", io.ErrShortWrite)
	default:
		return nil
	}
}

// Close releases the streams through their one owner, even when the reader and
// writer are two views of one resource.
func (c *ioConnection) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = c.release()
	})
	return c.closeErr
}
