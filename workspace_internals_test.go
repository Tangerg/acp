package acp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestTerminalReleaseCannotOvertakeAnAdmittedOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		terminal, stream := terminalOrderHarness(t)
		output := make(chan error, 1)
		go func() { _, err := terminal.Output(t.Context(), nil); output <- err }()
		<-stream.entered
		release := make(chan error, 1)
		go func() { _, err := terminal.Release(t.Context(), nil); release <- err }()
		synctest.Wait()
		select {
		case method := <-stream.written:
			t.Errorf("%s overtook the earlier operation", method)
		default:
		}
		close(stream.resume)
		if err := <-output; err != nil {
			t.Fatal(err)
		}
		if err := <-release; err != nil {
			t.Fatal(err)
		}
	})
}

func TestTerminalReleaseCanRunWhileWaitingForExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		terminal, stream := terminalOrderHarness(t)
		waiting := make(chan error, 1)
		go func() { _, err := terminal.WaitForExit(t.Context(), nil); waiting <- err }()
		id := <-stream.waiting
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := terminal.Release(ctx, nil); err != nil {
			t.Errorf("Release could not unblock a pending wait: %v", err)
		}
		stream.calls.deliver(&jsonrpc.Response{ID: id, Result: json.RawMessage(`{}`)})
		if err := <-waiting; err != nil {
			t.Fatal(err)
		}
	})
}

func terminalOrderHarness(t *testing.T) (*TerminalHandle, *terminalOrderStream) {
	t.Helper()
	conn := newTestAgentConn(t)
	conn.negotiation.accept(Negotiated{ClientCapabilities: ClientCapabilities{Terminal: true}})
	conn.negotiation.open()
	stream := &terminalOrderStream{
		calls: conn.calls, entered: make(chan struct{}), resume: make(chan struct{}),
		written: make(chan string, 2), waiting: make(chan jsonrpc.ID, 1),
	}
	conn.transport = stream
	terminal := &TerminalHandle{
		id: "terminal", session: &AgentSession{id: "session", conn: conn},
		state: &terminalState{sending: make(chan struct{}, 1)},
	}
	return terminal, stream
}

type terminalOrderStream struct {
	calls   *calls
	entered chan struct{}
	resume  chan struct{}
	written chan string
	waiting chan jsonrpc.ID
}

func (*terminalOrderStream) Read(context.Context) (jsonrpc.Message, error) { return nil, io.EOF }
func (*terminalOrderStream) Close() error                                  { return nil }

func (s *terminalOrderStream) Write(_ context.Context, message jsonrpc.Message) error {
	request := message.(*jsonrpc.Request)
	if !request.IsCall() {
		return nil
	}
	if request.Method == methodTerminalWaitForExit {
		s.waiting <- request.ID
		return nil
	}
	result := json.RawMessage(`{}`)
	if request.Method == methodTerminalOutput {
		close(s.entered)
		<-s.resume
		result = json.RawMessage(`{"output":"","truncated":false}`)
	}
	s.written <- request.Method
	s.calls.deliver(&jsonrpc.Response{ID: request.ID, Result: result})
	return nil
}
