package acp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// A connection that cannot write and a connection that cannot be released are
// both connections that have ended, and both used to say otherwise.

// A response this side cannot write ends the connection.
//
// Outbound calls routed their write failures through the terminal path and
// handler responses did not: the failure was logged and the connection stayed
// open, accepting requests it could never answer, with Wait blocked on a terminal
// state nothing was ever going to record.
func TestAResponseThatCannotBeWrittenEndsTheConnection(t *testing.T) {
	failure := errors.New("the output side is gone")
	transport := &failingWrites{
		failure: failure,
		// One request, then silence. The read side stays open, which is the case
		// that used to hang: a connection is only half broken.
		inbound: []string{`{"jsonrpc":"2.0","id":1,"method":"session/new",` +
			`"params":{"cwd":"/w","mcpServers":[]}}`},
	}

	conn, err := testAgent(t, nil).Connect(context.Background(), transport)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	waited := make(chan error, 1)
	go func() { waited <- conn.Wait() }()

	select {
	case err := <-waited:
		if !errors.Is(err, failure) {
			t.Fatalf("Wait reported %v, want the write failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned; the failed response write left no terminal state")
	}
}

// A transport this side cannot close is a failure to release what the connection
// owns, and Close and Wait both say so.
//
// This is the command transport's case: a subprocess that could not be reaped is
// still running, and a Close that answered nil would be reporting it as gone.
func TestATransportThatCannotBeClosedIsReported(t *testing.T) {
	failure := errors.New("the agent could not be reaped")
	transport := &failingClose{failure: failure}

	conn, err := testAgent(t, nil).Connect(context.Background(), transport)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}

	if err := conn.Close(); !errors.Is(err, failure) {
		t.Fatalf("Close reported %v, want the release failure", err)
	}
	// The same answer every time, from both, because a terminal condition that
	// depended on who asked first would be unusable.
	if err := conn.Close(); !errors.Is(err, failure) {
		t.Fatalf("the second Close reported %v", err)
	}
	if err := conn.Wait(); !errors.Is(err, failure) {
		t.Fatalf("Wait reported %v, want the release failure", err)
	}
}

// failingWrites delivers a fixed script of inbound messages, then blocks. Every
// write fails.
type failingWrites struct {
	failure error
	inbound []string

	mu     sync.Mutex
	sent   int
	closed chan struct{}
	once   sync.Once
}

func (t *failingWrites) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *failingWrites) Write(context.Context, jsonrpc.Message) error {
	return t.failure
}

func (t *failingWrites) Read(ctx context.Context) (jsonrpc.Message, error) {
	t.mu.Lock()
	if t.sent < len(t.inbound) {
		line := t.inbound[t.sent]
		t.sent++
		t.mu.Unlock()
		return jsonrpc.DecodeMessage([]byte(line))
	}
	t.mu.Unlock()

	select {
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *failingWrites) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// failingClose is a transport that reads nothing, writes nothing, and cannot be
// released.
type failingClose struct {
	failure error

	closed chan struct{}
	once   sync.Once
}

func (t *failingClose) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *failingClose) Write(context.Context, jsonrpc.Message) error { return nil }

func (t *failingClose) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *failingClose) Close() error {
	// The pending read is still released, because that is the one thing Close must
	// do however badly the rest of it goes.
	t.once.Do(func() { close(t.closed) })
	return t.failure
}

// A response this side cannot write in time ends the connection.
//
// The deadline on a response write is this package's own, not a caller's, so its
// expiry says the peer stopped reading. Treating it as a caller changing its mind
// left the connection alive with a request it had failed to answer.
func TestAResponseWriteThatTimesOutEndsTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A peer that asks and then stops reading. The write blocks until the
		// deadline this package set for it, which is what the test is about; under
		// the synthetic clock it costs nothing.
		transport := &scriptedWrites{
			inbound: []string{`{"jsonrpc":"2.0","id":1,"method":"session/new",` +
				`"params":{"cwd":"/w","mcpServers":[]}}`},
		}

		conn, err := testAgent(t, nil).Connect(context.Background(), transport)
		if err != nil {
			t.Fatalf("Agent.Connect: %v", err)
		}
		defer conn.Close() //nolint:errcheck // idempotent.

		if err := conn.Wait(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait reported %v, want the response write's own deadline", err)
		}
	})
}

// A transport that reports its output closed while its input still blocks ends the
// connection rather than waiting on a state transition nothing would start.
func TestAClosedOutputWithABlockedInputEndsTheConnection(t *testing.T) {
	// The handshake goes out, so the agent may send; everything after it is
	// refused, which is a transport whose output has gone while its input has not.
	transport := &scriptedWrites{
		inbound: []string{`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
			`"params":{"protocolVersion":1,"clientCapabilities":{}}}`},
		succeed: 1,
		failure: acp.ErrConnectionClosed,
	}

	conn, err := testAgent(t, nil).Connect(context.Background(), transport)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	failed := make(chan error, 1)
	go func() {
		failed <- conn.Notify(context.Background(), "_vendor.example/thing", nil)
	}()

	select {
	case err := <-failed:
		if !errors.Is(err, acp.ErrConnectionClosed) {
			t.Fatalf("Notify reported %v, want ErrConnectionClosed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Notify never returned; the closed output never became a terminal state")
	}
	if err := conn.Wait(); err != nil {
		t.Fatalf("Wait reported %v; a peer that hung up is not a failure", err)
	}
}

func TestACallerDeadlineBeforeCommitDoesNotEndTheConnection(t *testing.T) {
	for name, operation := range map[string]func(context.Context, *acp.AgentConn) error{
		"call": func(ctx context.Context, conn *acp.AgentConn) error {
			return conn.Call(ctx, "_vendor.example/thing", nil, nil)
		},
		"notification": func(ctx context.Context, conn *acp.AgentConn) error {
			return conn.Notify(ctx, "_vendor.example/thing", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := &scriptedWrites{
					inbound: []string{`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
						`"params":{"protocolVersion":1,"clientCapabilities":{}}}`},
					succeed:                  1,
					succeedAfterContextError: true,
				}
				conn, err := testAgent(t, nil).Connect(context.Background(), transport)
				if err != nil {
					t.Fatalf("Agent.Connect: %v", err)
				}

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := operation(ctx, conn); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("operation reported %v, want its write deadline", err)
				}
				if err := conn.Notify(context.Background(), "_vendor.example/after", nil); err != nil {
					t.Fatalf("the next write failed after a message that was never committed: %v", err)
				}
				if err := conn.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				if err := conn.Wait(); err != nil {
					t.Fatalf("Wait reported %v after a healthy local close", err)
				}
			})
		})
	}
}

func TestAWrappedCallerDeadlineEndsTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &scriptedWrites{
			inbound: []string{`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
				`"params":{"protocolVersion":1,"clientCapabilities":{}}}`},
			succeed:     1,
			wrapContext: true,
		}
		conn, err := testAgent(t, nil).Connect(context.Background(), transport)
		if err != nil {
			t.Fatalf("Agent.Connect: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Notify(ctx, "_vendor.example/thing", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Notify reported %v, want its write deadline", err)
		}
		if err := conn.Wait(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait reported %v, want the uncertain write failure", err)
		}
	})
}

// scriptedWrites delivers a fixed list of inbound messages and then blocks its
// reads. The first succeed writes go through; the next one fails with failure or
// its context, and a recovery test may let later writes through.
type scriptedWrites struct {
	inbound                  []string
	succeed                  int
	failure                  error
	succeedAfterContextError bool
	wrapContext              bool

	mu      sync.Mutex
	read    int
	written int
	closed  chan struct{}
	once    sync.Once
}

func (t *scriptedWrites) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *scriptedWrites) Write(ctx context.Context, _ jsonrpc.Message) error {
	t.mu.Lock()
	t.written++
	attempt := t.written
	t.mu.Unlock()
	if attempt <= t.succeed || t.succeedAfterContextError && attempt > t.succeed+1 {
		return nil
	}
	if t.failure != nil {
		return t.failure
	}
	select {
	case <-ctx.Done():
		if t.wrapContext {
			return fmt.Errorf("the message may have been committed: %w", ctx.Err())
		}
		return ctx.Err()
	case <-t.closed:
		return acp.ErrConnectionClosed
	}
}

func (t *scriptedWrites) Read(ctx context.Context) (jsonrpc.Message, error) {
	t.mu.Lock()
	if t.read < len(t.inbound) {
		line := t.inbound[t.read]
		t.read++
		t.mu.Unlock()
		return jsonrpc.DecodeMessage([]byte(line))
	}
	t.mu.Unlock()

	select {
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *scriptedWrites) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}
