package acp

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/Tangerg/acp/jsonrpc"
)

// A Transport establishes one logical ACP connection.
type Transport interface {
	// Connect establishes the connection. The context scopes establishing it,
	// not the connection's lifetime.
	Connect(ctx context.Context) (Connection, error)
}

// A Connection is an established bidirectional message stream.
//
// Custom transports satisfy these rules:
//
//   - A transport is connected at most once.
//   - Write may be called concurrently. Read is called by one goroutine at a
//     time, concurrently with Write and Close.
//   - Close is idempotent and safe to call concurrently, and it makes a pending
//     Read and Write return. Otherwise the connection cannot stop.
//   - Read returns a [*jsonrpc.DecodeError] for one message that framing
//     delivered but JSON-RPC cannot decode; the connection answers it and keeps
//     reading. Any other error ends the connection, and io.EOF means the peer
//     closed the stream cleanly.
//   - Write may return exactly ctx.Err only before any part of the message is
//     committed, which is the transport's promise that the connection remains
//     usable. Once a message is partly written it is finished or the transport
//     fails, because a byte stream cannot take back half a message.
type Connection interface {
	Read(ctx context.Context) (jsonrpc.Message, error)
	Write(ctx context.Context, message jsonrpc.Message) error
	Close() error
}

// singleUse makes a transport connectable once, and reports which concurrent
// Connect claimed it.
type singleUse struct{ used atomic.Bool }

func (s *singleUse) claim() error {
	if s.used.CompareAndSwap(false, true) {
		return nil
	}
	return errTransportUsed
}

var errTransportUsed = errors.New("acp: transport is already connected")
