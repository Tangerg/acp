package acp

import (
	"context"
	"io"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// NewInMemoryTransports returns two transports connected to each other: a message
// written to one is read from the other.
//
// This is what the rest of this package is tested over. A protocol that only
// works when one end is a subprocess is one whose tests need a subprocess, and
// then the tests are slow, platform-dependent, and hard to make deterministic —
// so the connection machinery is written against a transport it can be driven
// through directly, and `os` stays out of everything but the transports that need
// it.
//
// It is not a toy. Two peers in one process is a real deployment: an editor that
// embeds an agent, or a test harness that drives both ends of a turn.
func NewInMemoryTransports() (client, agent Transport) {
	toAgent := newMessagePipe()
	toClient := newMessagePipe()
	return &inMemoryTransport{read: toClient, write: toAgent},
		&inMemoryTransport{read: toAgent, write: toClient}
}

type inMemoryTransport struct {
	singleUse
	read  *messagePipe
	write *messagePipe
}

func (t *inMemoryTransport) Connect(context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	return &inMemoryConnection{read: t.read, write: t.write}, nil
}

type inMemoryConnection struct {
	read  *messagePipe
	write *messagePipe

	closeOnce sync.Once
}

func (c *inMemoryConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	return c.read.receive(ctx)
}

func (c *inMemoryConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	return c.write.send(ctx, message)
}

// Close closes both directions. Closing only the write side would leave the peer
// able to send into a connection nobody is reading, and a pending Read on this
// side blocked forever — and unblocking a pending Read is the one thing the
// Connection contract insists Close does.
func (c *inMemoryConnection) Close() error {
	c.closeOnce.Do(func() {
		c.read.close()
		c.write.close()
	})
	return nil
}

// A messagePipe carries messages one way.
//
// Messages rather than bytes, deliberately: an in-memory transport that encoded
// and re-decoded would be testing the codec a second time and hiding a framing
// bug rather than exposing one. Framing is the byte-stream transports' business.
type messagePipe struct {
	messages chan jsonrpc.Message
	closed   chan struct{}
	once     sync.Once
}

func newMessagePipe() *messagePipe {
	return &messagePipe{
		// Unbuffered: a send completes when the peer takes the message, which
		// makes the ordering in a test the ordering on the wire.
		messages: make(chan jsonrpc.Message),
		closed:   make(chan struct{}),
	}
}

func (p *messagePipe) send(ctx context.Context, message jsonrpc.Message) error {
	select {
	case p.messages <- message:
		return nil
	case <-p.closed:
		return ErrConnectionClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *messagePipe) receive(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case message := <-p.messages:
		return message, nil
	case <-p.closed:
		// A closed pipe with nothing left in it is a clean end of stream, which is
		// what the connection reports as a peer that hung up rather than as a
		// failure.
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *messagePipe) close() {
	p.once.Do(func() { close(p.closed) })
}
