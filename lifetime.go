package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// A lifetime owns shutdown: transport release, reader exit, delivery completion,
// and the terminal error. These are separate moments because closing a stream
// may race a successful Read that has not yet queued its last message; only the
// reader signals its exit, and only delivery signals that every queued message
// has reached its handler or waiter.
type lifetime struct {
	// ctx is the connection's lifetime, which outlives every context passed to
	// it: a five-second handshake timeout is not a five-second connection.
	ctx    context.Context //nolint:containedctx // the connection's lifetime, not a call's.
	cancel context.CancelFunc

	endOnce   sync.Once
	released  chan struct{}
	readEnded chan struct{}
	delivered chan struct{}
	// terminal and releaseErr are written before released closes and read only
	// after, so every reader sees the same values without a lock.
	terminal   error
	releaseErr error

	mu         sync.Mutex
	unregister func()
	// stopped closes the work pool once delivery has finished, because adding to
	// a WaitGroup that Wait has begun draining is a data race.
	stopped bool
	work    sync.WaitGroup
}

func newLifetime() *lifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifetime{
		ctx:       ctx,
		cancel:    cancel,
		released:  make(chan struct{}),
		readEnded: make(chan struct{}),
		delivered: make(chan struct{}),
	}
}

// endReading ends the connection once and reports whether this call did. A clean
// end of stream and a local close are not failures. Cancellation precedes release
// so that a transport whose Close waits for handler work cannot wait on work that
// is waiting for this context; a failed release is terminal, because a subprocess
// that could not be reaped is still running.
func (l *lifetime) endReading(cause error, release func() error) bool {
	ended := false
	l.endOnce.Do(func() {
		ended = true
		if errors.Is(cause, io.EOF) || errors.Is(cause, ErrConnectionClosed) {
			cause = nil
		}
		l.cancel()
		l.releaseErr = release()
		l.terminal = cause
		if l.terminal == nil {
			l.terminal = l.releaseErr
		}

		l.mu.Lock()
		if l.unregister != nil {
			l.unregister()
			l.unregister = nil
		}
		close(l.released)
		l.mu.Unlock()
	})
	return ended
}

// onRelease runs unregister when the connection is released, or now if it has
// been already.
func (l *lifetime) onRelease(unregister func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended() {
		unregister()
		return
	}
	l.unregister = unregister
}

func (l *lifetime) finishDelivering(release func()) {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()

	release()
	close(l.delivered)
}

// start runs fn as work Wait owns, unless delivery has already finished, when no
// peer could observe it and it is not started.
func (l *lifetime) start(fn func()) {
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

// ended reports whether the transport has been released.
func (l *lifetime) ended() bool {
	select {
	case <-l.released:
		return true
	default:
		return false
	}
}

func (l *lifetime) wait() error {
	<-l.delivered
	l.work.Wait()
	<-l.released
	return l.terminal
}

// failure is the error an operation on an ended connection returns. It does not
// wait for delivery: a slow notification handler must not hold a failed write
// after the transport is already closed.
func (l *lifetime) failure() error {
	<-l.released
	if l.terminal != nil {
		return fmt.Errorf("%w: %w", ErrConnectionClosed, l.terminal)
	}
	return ErrConnectionClosed
}
