package acp_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// The lifecycle properties a connection has to have, each stated as the thing
// that would otherwise go wrong.

// A response this connection already read is an answer, even if the peer hung up
// in the same breath.
//
// The read side ending and everything it accepted having been delivered are two
// facts. When they were one, a call whose answer was still in the delivery queue
// could be told the connection had closed instead — and because both channels
// were ready at once, which it got depended on the scheduler. That is the shape
// of bug a test has to make deterministic rather than hope to catch.
func TestAnAnswerAlreadyReadBeatsTheEndOfTheStream(t *testing.T) {
	// Repeated because the failure it guards against was a race: one run proves
	// little, and the ordering here is deterministic, so many runs cost nothing.
	for range 200 {
		client := testClient(t)
		transport := &answerThenEOF{}

		conn, err := client.Connect(context.Background(), transport)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if version := conn.Peer().ProtocolVersion; version != acp.CurrentProtocolVersion {
			t.Fatalf("negotiated version %d", version)
		}
		if err := conn.Wait(); err != nil {
			t.Fatalf("Wait reported %v, want nil after a clean end of stream", err)
		}
	}
}

// answerThenEOF answers the first request and then ends the stream, with no
// scheduling gap between the two.
type answerThenEOF struct {
	mu       sync.Mutex
	answered bool
	replies  []jsonrpc.Message
	closed   chan struct{}
	once     sync.Once
}

func (t *answerThenEOF) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *answerThenEOF) Write(_ context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok || !request.IsCall() {
		return nil
	}
	response, err := answerInitialize(request)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.replies = append(t.replies, response)
	t.answered = true
	t.mu.Unlock()
	return nil
}

func (t *answerThenEOF) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		t.mu.Lock()
		if len(t.replies) > 0 {
			reply := t.replies[0]
			t.replies = t.replies[1:]
			t.mu.Unlock()
			return reply, nil
		}
		answered := t.answered
		t.mu.Unlock()

		if answered {
			// The queue is empty and the answer has been handed over. Ending the
			// stream now is exactly the ordering that used to lose it.
			return nil, io.EOF
		}
		select {
		case <-t.closed:
			return nil, io.EOF
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
}

func (t *answerThenEOF) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func answerInitialize(request *jsonrpc.Request) (jsonrpc.Message, error) {
	const result = `{"protocolVersion":1}`
	encoded, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: request.ID, Result: []byte(result)})
	if err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(encoded)
}

// A cancelled call returns at its own cancellation, not at the peer's
// convenience.
//
// Telling the peer to stop is courtesy on an independent budget, and adding that
// budget to a caller that has already given up would make every cancelled request
// take five seconds.
func TestACancelledCallDoesNotWaitForThePeerToBeTold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testClient(t)
		transport := &blockingCancelWrites{released: make(chan struct{})}

		conn, err := client.Connect(context.Background(), transport)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		returned := make(chan error, 1)
		go func() {
			returned <- conn.Call(ctx, "_vendor.example/slow", nil, nil)
		}()

		synctest.Wait()
		start := time.Now()
		cancel()

		if err := <-returned; !errors.Is(err, context.Canceled) {
			t.Fatalf("the call returned %v, want context.Canceled", err)
		}
		// Within the synthetic clock, "immediately" is exact: any wait on the
		// blocked cancellation write would show up as elapsed time.
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the caller waited %v for the peer to be told", waited)
		}

		close(transport.released)
		if err := conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// blockingCancelWrites answers initialize and then blocks any write of a
// cancellation, which is what an unresponsive peer looks like from here.
type blockingCancelWrites struct {
	released chan struct{}

	mu      sync.Mutex
	replies []jsonrpc.Message
	closed  chan struct{}
	once    sync.Once
}

func (t *blockingCancelWrites) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *blockingCancelWrites) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		return nil
	}
	if strings.HasPrefix(request.Method, "$/") {
		select {
		case <-t.released:
		case <-ctx.Done():
			return ctx.Err()
		case <-t.closed:
			return acp.ErrConnectionClosed
		}
		return nil
	}
	if request.Method != "initialize" {
		return nil // the slow call is never answered
	}
	response, err := answerInitialize(request)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.replies = append(t.replies, response)
	t.mu.Unlock()
	return nil
}

func (t *blockingCancelWrites) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		t.mu.Lock()
		if len(t.replies) > 0 {
			reply := t.replies[0]
			t.replies = t.replies[1:]
			t.mu.Unlock()
			return reply, nil
		}
		t.mu.Unlock()

		select {
		case <-t.closed:
			return nil, io.EOF
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (t *blockingCancelWrites) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}
