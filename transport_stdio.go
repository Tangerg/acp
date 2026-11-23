package acp

import (
	"context"
	"os"
)

// A StdioTransport is the agent's side of a local connection: this process's own
// standard input and output. Standard output carries only protocol messages, so
// an agent writes its diagnostics to standard error.
//
// The process owns its standard streams, so closing the connection does not
// close them. Inherited streams are usually blocking, which closing would not
// interrupt anyway; the connection abandons a blocked read or write at Close
// instead, so [Agent.Run] returns when its context ends even while the client
// holds the pipe open.
type StdioTransport struct {
	singleUse
}

// NewStdioTransport returns a transport over this process's standard streams.
func NewStdioTransport() *StdioTransport {
	return new(StdioTransport)
}

// Connect opens this process's standard streams as the protocol stream. A
// transport connects once.
func (t *StdioTransport) Connect(ctx context.Context) (Connection, error) {
	if err := t.claim(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newIOConnection(IOConfig{
		Reader: os.Stdin,
		Writer: os.Stdout,
		Close:  func() error { return nil },
	}, true), nil
}
