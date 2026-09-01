package acp_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
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
