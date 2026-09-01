package acp

import (
	"context"
	"errors"
	"io"
	"sync"
)

// A lifetime is a connection's own: when it stopped reading, when everything it
// had already accepted had been delivered, and why it ended.
//
// The two moments are separate on purpose, and were once one channel. A response
// the read side has accepted and queued is an answer this connection holds, and a
// call waiting for it must not be told the connection ended instead — which is
// what happened when the peer hung up in the same breath as answering, because
// both facts were ready at once and select chose between them at random.
type lifetime struct {
	// ctx scopes everything the connection owns: the read loop, and every inbound
	// request's context.
	//
	// A context in a struct is ordinarily a smell, because it hides which call a
	// deadline belongs to. This one is not a call's: it is the connection's, which
	// outlives every context passed to it — a caller who gave Connect a
	// five-second handshake timeout has not asked for the connection to die after
	// five seconds. There is nowhere else for it to live.
	ctx    context.Context //nolint:containedctx // the connection's lifetime, not a call's.
	cancel context.CancelFunc

	endOnce   sync.Once
	readEnded chan struct{}
	delivered chan struct{}
	// terminal is written before readEnded closes and read only after, so every
	// caller sees the same value without holding a lock.
	terminal error

	mu sync.Mutex
	// stopped closes the work pool to new entrants. It is a separate flag from the
	// channels because it guards a WaitGroup, and adding to one that Wait has
	// already begun draining is a data race rather than a late arrival.
	stopped bool
	work    sync.WaitGroup
}

func newLifetime() *lifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifetime{
		ctx:       ctx,
		cancel:    cancel,
		readEnded: make(chan struct{}),
		delivered: make(chan struct{}),
	}
}

// Cancellation precedes release so that a transport whose Close waits for handler
// work cannot form a cycle with work still waiting for its context. Release runs
// inside because failing it is terminal rather than a detail for a log: a
// subprocess that could not be reaped is still running, and answering "closed
// cleanly" would report it as gone.
func (l *lifetime) endReading(cause error, release func() error) {
	l.endOnce.Do(func() {
		// A clean end of stream is not a failure. Neither is a read that was
		// unblocked by this side closing the transport.
		switch {
		case errors.Is(cause, io.EOF):
			cause = nil
		case errors.Is(cause, context.Canceled) && l.ctx.Err() != nil:
			cause = nil
		case errors.Is(cause, ErrConnectionClosed):
			cause = nil
		}
		l.cancel()
		if err := release(); err != nil && cause == nil {
			cause = err
		}
		l.terminal = cause

		close(l.readEnded)
	})
}

func (l *lifetime) finishDelivering(release func()) {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()

	release()
	close(l.delivered)
}

// Once delivery has shut the work pool no peer can observe newly scheduled work,
// so starting any would create a goroutine Wait no longer owns.
func (l *lifetime) spawn(fn func()) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.work.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.work.Done()
		fn()
	}()
}

func (l *lifetime) run(fn func()) { l.work.Go(fn) }

// Over means the read side has finished, not that the queue has drained: a
// connection that will accept nothing further is not open, however much is still
// being handed to callers already waiting.
func (l *lifetime) ended() bool {
	select {
	case <-l.readEnded:
		return true
	default:
		return false
	}
}

func (l *lifetime) wait() error {
	<-l.delivered
	l.work.Wait()
	return l.terminal
}

func (l *lifetime) failure() error {
	// terminal is final before readEnded closes. Waiting for delivery here would
	// let an unrelated slow notification handler hold a failed write — and its
	// caller's deadline — after the transport is already irrecoverably closed.
	<-l.readEnded
	if l.terminal != nil {
		return l.terminal
	}
	return ErrConnectionClosed
}
