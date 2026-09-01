package acp

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/Tangerg/acp/jsonrpc"
)

// A Transport is something a logical connection can be established over.
//
// The interface is the MCP Go SDK's, unchanged, because it is the minimum a
// bidirectional JSON-RPC link needs and the easiest thing for a caller to
// implement. Nothing in this package requires a subprocess: anything that can
// carry messages in both directions will do, which is what makes the same code
// testable over an in-memory pipe.
type Transport interface {
	// Connect establishes the connection. The context scopes establishing it and
	// not the connection's lifetime: a caller who passed a five-second dial
	// timeout has not asked for the connection to die after five seconds.
	Connect(ctx context.Context) (Connection, error)
}

// A Connection is an established bidirectional message stream.
//
// The signatures are the easy half. What a custom transport is actually being
// asked to promise is this, and it is stated rather than left in a comment on one
// implementation:
//
//   - A transport is connected at most once.
//   - Write may be called concurrently, from any number of goroutines.
//   - Read may run concurrently with Close. It is not called concurrently with
//     itself: one goroutine owns reading.
//   - Close is idempotent and safe to call concurrently, and it unblocks a
//     pending Read. Without that last part, a Read and the goroutine blocked in
//     it cannot be stopped, which is why this interface takes a closeable stream
//     pair rather than a bare reader and writer.
//   - A failed read or write ends the logical connection. The failure is the
//     connection's terminal error, and every caller of Wait sees the same one.
//
// Untested concurrency contracts are not contracts, so these are tested with
// testing/synctest rather than with sleeps.
type Connection interface {
	// Read returns the next message, or an error. io.EOF means the peer closed
	// the stream cleanly.
	Read(ctx context.Context) (jsonrpc.Message, error)

	// Write sends a message.
	Write(ctx context.Context, message jsonrpc.Message) error

	// Close releases the stream and unblocks a pending Read.
	Close() error
}

// A singleUse enforces the first clause of the Connection contract for the
// transports in this package.
//
// It is a type rather than the same two lines in three places because the obvious
// spelling of it is wrong. A sync.Once that sets a flag, followed by a read of
// that flag outside the once, lets two concurrent Connect calls both see it set:
// the second call's Do returns as soon as the first has finished, and neither
// knows which of them ran it.
type singleUse struct{ used atomic.Bool }

func (s *singleUse) claim() error {
	if s.used.CompareAndSwap(false, true) {
		return nil
	}
	return errTransportUsed
}

var errTransportUsed = errors.New("acp: a transport is connected at most once, and this one already has been")
