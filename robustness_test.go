package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// A peer at the other end of a real byte stream, which is where an undecodable
// message can exist at all: a message transport carries only decoded messages.
type bytePeer struct {
	lines  *bufio.Reader
	writer io.Writer
}

func newBytePeer(t *testing.T) (acp.Transport, *bytePeer) {
	t.Helper()
	localRead, peerWrite := io.Pipe()
	peerRead, localWrite := io.Pipe()
	t.Cleanup(func() { _ = errors.Join(peerRead.Close(), peerWrite.Close()) })
	transport := acp.NewIOTransport(&acp.IOConfig{
		Reader: localRead, Writer: localWrite,
		Close: func() error { return errors.Join(localRead.Close(), localWrite.Close()) },
	})
	return transport, &bytePeer{lines: bufio.NewReader(peerRead), writer: peerWrite}
}

func (p *bytePeer) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(p.writer, line+"\n"); err != nil {
		t.Fatalf("write %s: %v", line, err)
	}
}

func (p *bytePeer) receive(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	line, err := p.lines.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return envelope
}

func errorCode(t *testing.T, envelope map[string]json.RawMessage) acp.ErrorCode {
	t.Helper()
	var failure acp.Error
	if err := json.Unmarshal(envelope["error"], &failure); err != nil {
		t.Fatalf("the response carries no error: %v", envelope)
	}
	return failure.Code
}

// An undecodable message is answered with an id-less error, as the reference
// implementation does, and the connection keeps reading.
func TestAnUndecodableMessageIsAnsweredAndReadingContinues(t *testing.T) {
	transport, peer := newBytePeer(t)
	conn, err := testAgent(t, nil).Connect(t.Context(), transport)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	for line, want := range map[string]acp.ErrorCode{
		`not json`:                         acp.ErrorCodeParseError,
		`[{"jsonrpc":"2.0","method":"x"}]`: acp.ErrorCodeInvalidRequest,
		`{"jsonrpc":"1.0","id":1,"method":"initialize"}`:   acp.ErrorCodeInvalidRequest,
		`{"jsonrpc":"2.0","id":1.5,"method":"initialize"}`: acp.ErrorCodeInvalidRequest,
	} {
		peer.send(t, line)
		answer := peer.receive(t)
		if string(answer["id"]) != "null" || errorCode(t, answer) != want {
			t.Fatalf("%s was answered %v, want error %d with a null id", line, answer, want)
		}
	}
	peer.send(t, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}`)
	if answer := peer.receive(t); answer["error"] != nil {
		t.Fatalf("the connection stopped serving after the malformed messages: %v", answer)
	}
}

// A malformed response fails only the call it names.
func TestAMalformedResponseFailsOnlyItsCall(t *testing.T) {
	transport, peer := newBytePeer(t)
	connected := make(chan *acp.ClientConn, 1)
	go func() {
		conn, err := testClient(t).Connect(context.Background(), transport)
		if err != nil {
			t.Error(err)
		}
		connected <- conn
	}()
	opening := peer.receive(t)
	peer.send(t, `{"jsonrpc":"2.0","id":`+string(opening["id"])+`,"result":{"protocolVersion":1}}`)
	conn := <-connected
	if conn == nil {
		t.FailNow()
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	failed := make(chan error, 1)
	go func() { failed <- conn.Call(context.Background(), "_test/first", nil, nil) }()
	first := peer.receive(t)
	peer.send(t, `{"jsonrpc":"2.0","id":`+string(first["id"])+`,"result":{},"error":{"code":1,"message":"x"}}`)
	if err := <-failed; err == nil {
		t.Fatal("a call answered with both a result and an error succeeded")
	}

	succeeded := make(chan error, 1)
	go func() { succeeded <- conn.Call(context.Background(), "_test/second", nil, nil) }()
	second := peer.receive(t)
	peer.send(t, `{"jsonrpc":"2.0","id":`+string(second["id"])+`,"result":{}}`)
	if err := <-succeeded; err != nil {
		t.Fatalf("the connection did not survive a malformed response: %v", err)
	}
}

// Deleting a session the agent does not have leaves the end state the caller
// asked for, so resource-not-found is a success. Close follows the same rule.
func TestDeletingAMissingSessionSucceeds(t *testing.T) {
	ctx := t.Context()
	clientSide, agentSide := acp.NewInMemoryTransports()
	agentStream, err := agentSide.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer agentStream.Close() //nolint:errcheck // test cleanup.
	connected := make(chan *acp.ClientConn, 1)
	go func() {
		client, connectErr := testClient(t).Connect(ctx, clientSide)
		if connectErr != nil {
			t.Error(connectErr)
		}
		connected <- client
	}()
	opening := expectCall(ctx, t, agentStream, "initialize")
	answerCall(ctx, t, agentStream, opening,
		`{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{},"delete":{}}}}`)
	client := <-connected
	if client == nil {
		t.FailNow()
	}
	defer client.Close() //nolint:errcheck // test cleanup.

	notFound := `{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"Resource not found"}}`
	deleted := make(chan error, 1)
	go func() {
		_, deleteErr := client.DeleteSession(ctx, &acp.DeleteSessionRequest{SessionID: "gone"})
		deleted <- deleteErr
	}()
	request := expectCall(ctx, t, agentStream, "session/delete")
	writeRaw(ctx, t, agentStream, strings.Replace(notFound, "%s", idOf(t, request), 1))
	if err := <-deleted; err != nil {
		t.Fatalf("deleting a missing session failed: %v", err)
	}
}

type failingRead struct {
	acp.Connection
	failure error
}

func (f *failingRead) Read(context.Context) (jsonrpc.Message, error) { return nil, f.failure }

type failingReadTransport struct{ failure error }

func (t *failingReadTransport) Connect(ctx context.Context) (acp.Connection, error) {
	_, agentSide := acp.NewInMemoryTransports()
	stream, err := agentSide.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &failingRead{Connection: stream, failure: t.failure}, nil
}

// An operation on a connection that ended because of a failure reports both that
// the connection is closed and why.
func TestAnOperationAfterAFailureWrapsTheCause(t *testing.T) {
	failure := errors.New("the stream broke")
	conn, err := testAgent(t, nil).Connect(t.Context(), &failingReadTransport{failure: failure})
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := conn.Wait(); !errors.Is(waitErr, failure) {
		t.Fatalf("Wait = %v, want the read failure", waitErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = conn.Notify(ctx, "_test/after", nil)
	if !errors.Is(err, acp.ErrConnectionClosed) || !errors.Is(err, failure) {
		t.Fatalf("an operation after the failure returned %v, want ErrConnectionClosed wrapping the cause", err)
	}
}
