package acp

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/Tangerg/acp/jsonrpc"
)

// A Transport establishes one logical ACP connection.
type Transport interface {
	// Connect establishes the connection. The context scopes establishing it and
	// not the connection's lifetime: a caller who passed a five-second dial
	// timeout has not asked for the connection to die after five seconds.
	Connect(ctx context.Context) (Connection, error)
}

// A Connection is an established bidirectional message stream.
//
// Custom transports must satisfy these lifecycle and concurrency rules:
//
//   - A transport is connected at most once.
//   - Write may be called concurrently, from any number of goroutines.
//   - Read may run concurrently with Close. It is not called concurrently with
//     itself: one goroutine owns reading.
//   - Close is idempotent and safe to call concurrently, and it unblocks a
//     pending Read. Without that last part, a Read and the goroutine blocked in
//     it cannot be stopped, which is why this interface takes a closeable stream
//     pair rather than a bare reader and writer.
//   - A failed read ends the logical connection. A failed write does too, except
//     when it returns exactly ctx.Err: that is the transport's promise that it
//     committed no part of the message and the connection remains usable. A
//     transport whose commit state is uncertain must return another error, even
//     when cancellation caused it. A terminal failure is the connection's error,
//     and every caller of Wait sees the same one.
type Connection interface {
	// Read returns the next message, or an error. io.EOF means the peer closed
	// the stream cleanly.
	Read(ctx context.Context) (jsonrpc.Message, error)

	// Write sends a message. Returning exactly ctx.Err reports that no part of the
	// message was committed; all other errors may be terminal.
	Write(ctx context.Context, message jsonrpc.Message) error

	// Close releases the stream and unblocks a pending Read.
	Close() error
}

// singleUse identifies which concurrent Connect call claimed a transport.
// sync.Once cannot report which caller executed its function.
type singleUse struct{ used atomic.Bool }

func (s *singleUse) claim() error {
	if s.used.CompareAndSwap(false, true) {
		return nil
	}
	return errTransportUsed
}

var errTransportUsed = errors.New("acp: transport is already connected")
