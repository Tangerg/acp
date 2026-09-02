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
// Messages pass directly without JSON encoding or framing. Use these transports
// for embedded peers and deterministic connection tests.
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

func (c *inMemoryConnection) Close() error {
	// Both directions. Closing only the write side would leave a pending Read here
	// blocked for ever, which is the one thing the Connection contract insists
	// Close does not do.
	c.closeOnce.Do(func() {
		c.read.close()
		c.write.close()
	})
	return nil
}

// messagePipe carries messages directly so connection tests remain independent
// of byte framing and JSON encoding.
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
