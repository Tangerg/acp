package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

// The inbound half of elicitation, which no agent built on this package can
// produce: the outbound gate refuses a mode the client did not advertise before
// the write, so reaching the client's dispatcher with one takes a raw peer.
//
// It is worth reaching. A capability is an authority boundary and this package
// enforces it in both directions, so the direction an honest peer never exercises
// is exactly the one a test has to.

// A mode the client never advertised is refused by the client too, and the
// refusal names the capability rather than claiming the method is missing.
func TestAnUnadvertisedModeIsRefusedInbound(t *testing.T) {
	var served int
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{
			Form: func(
				context.Context,
				*CreateElicitationRequest,
				*ElicitationFormMode,
			) (*CreateElicitationResponse, error) {
				served++
				return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	stream, conn := connectRawClient(t, client)
	defer conn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A url elicitation, which this client advertised no url mode for.
	url := `{"message":"sign in","mode":"url","elicitationId":"e-1",` +
		`"url":"https://example.invalid/","sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(7, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write the url elicitation: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
		t.Errorf("an unadvertised mode was answered %v, want invalid params", code)
	}

	// A mode this package cannot name at all, which is how one from a later schema
	// arrives. Guessing which of the two it resembles would be worse than saying so.
	unknown := `{"message":"?","mode":"telepathy","sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(8, methodElicitationCreate, unknown)); err != nil {
		t.Fatalf("write the unknown mode: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
		t.Errorf("an unknown mode was answered %v, want invalid params", code)
	}

	if served != 0 {
		t.Error("a refused elicitation still reached a handler")
	}
}

// A client that serves no elicitation at all refuses the method rather than the
// mode: it advertised nothing, so the agent was told the method was not there.
func TestElicitationIsMethodNotFoundWithoutHandlers(t *testing.T) {
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	stream, conn := connectRawClient(t, client)
	defer conn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	form := `{"message":"?","mode":"form","requestedSchema":{"type":"object","properties":{}},` +
		`"sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(7, methodElicitationCreate, form)); err != nil {
		t.Fatalf("write the form elicitation: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
		t.Errorf("an unadvertised method was answered %v, want method not found", code)
	}
}

// connectRawClient connects a real client to a hand-driven agent, answering the
// handshake the client performs so that Connect returns.
func connectRawClient(t *testing.T, client *Client) (Connection, *ClientConn) {
	t.Helper()
	clientSide, agentSide := NewInMemoryTransports()
	ctx := context.Background()

	stream, err := agentSide.Connect(ctx)
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// Client.Connect blocks until initialize is answered, so the answer has to be
	// written from somewhere else.
	answered := make(chan error, 1)
	go func() {
		request, readErr := stream.Read(ctx)
		if readErr != nil {
			answered <- readErr
			return
		}
		call, ok := request.(*jsonrpc.Request)
		if !ok || call.Method != methodInitialize {
			answered <- errNotInitialize
			return
		}
		// An agent advertising nothing. What this client can be asked for is
		// decided by what the client advertised, which is the point of the test.
		result, encodeErr := json.Marshal(&InitializeResponse{ProtocolVersion: CurrentProtocolVersion})
		if encodeErr != nil {
			answered <- encodeErr
			return
		}
		answered <- stream.Write(ctx, &jsonrpc.Response{ID: call.ID, Result: result})
	}()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("answering the handshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return stream, conn
}

var errNotInitialize = newError(ErrorCodeInvalidRequest, "the client did not open with initialize")

func denyingPermissionInternal(
	context.Context,
	*RequestPermissionRequest,
) (*RequestPermissionResponse, error) {
	return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
}

// The observed client, held against the gate.
//
// design.md recorded this gap as one that affected a real editor: Zed advertises
// both elicitation modes, and an agent built on this package could not use either.
// The transcript is the evidence the gap is closed, and it is the editor's own
// recorded advertisement rather than a capability object written here to agree
// with the answer.
func TestTheRecordedEditorCanBeElicitedFrom(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "zed", "terminal-and-cancellation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript struct {
		ClientToAgent []json.RawMessage `json:"clientToAgent"`
	}
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatal(err)
	}

	var peer PeerInfo
	for _, message := range transcript.ClientToAgent {
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(message, &envelope); err != nil || envelope.Method != methodInitialize {
			continue
		}
		var request InitializeRequest
		if err := json.Unmarshal(envelope.Params, &request); err != nil {
			t.Fatalf("decoding the editor's initialize: %v", err)
		}
		peer = PeerInfo{ClientCapabilities: request.ClientCapabilities}
	}
	if !hasCapability(peer.ClientCapabilities.Elicitation) {
		t.Fatal("the transcript no longer carries an initialize advertising elicitation")
	}

	for _, method := range []string{methodElicitationCreate, methodElicitationComplete} {
		if err := peer.permits(method); err != nil {
			t.Errorf("the editor advertised %s and the gate refused it: %v", method, err)
		}
	}
	for name, mode := range map[string]CreateElicitationRequestValue{
		"form": &ElicitationFormMode{},
		"url":  &ElicitationURLMode{},
	} {
		if err := peer.permitsElicitationMode(mode); err != nil {
			t.Errorf("the editor advertised the %s mode and the gate refused it: %v", name, err)
		}
	}
}
