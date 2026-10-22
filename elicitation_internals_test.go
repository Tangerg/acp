package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/internal/wire"
	"github.com/Tangerg/acp/jsonrpc"
)

func TestAScopedElicitationWriteSharesItsParentsResponseDeadline(t *testing.T) {
	for _, readElicitation := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked elicitation", true: "blocked response"}[readElicitation], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := newTestAgentConn()
				conn.handshake.begin()
				conn.handshake.accept(PeerInfo{ClientCapabilities: ClientCapabilities{
					Elicitation: OptValue(ElicitationCapabilities{Form: OptValue(ElicitationFormCapabilities{})}),
				}})
				conn.handshake.publish(nil)
				transport, peerTransport := NewInMemoryTransports()
				stream, err := transport.Connect(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				peer, err := peerTransport.Connect(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				blocked := &blockedElicitationStream{Connection: stream, entered: make(chan struct{})}
				conn.transport = blocked
				conn.run()
				defer conn.endReading(nil)
				parent, err := conn.requests.accept(conn.life.ctx, jsonrpc2.Int64ID(9))
				if err != nil {
					t.Fatal(err)
				}
				ctx := withServingRequest(context.WithoutCancel(parent.ctx), parent, methodAuthenticate)
				elicited := make(chan error, 1)
				go func() {
					_, err := conn.CreateElicitation(ctx, &CreateElicitationParams{
						Message: "?", Mode: &ElicitationFormMode{RequestedSchema: ElicitationSchema{
							Type: ElicitationSchemaTypeObject, Properties: map[string]ElicitationPropertySchema{},
						}},
					})
					elicited <- err
				}()
				<-blocked.entered
				answered := make(chan bool, 1)
				go func() {
					defer parent.finish()
					answered <- conn.respond(parent, &AuthenticateResponse{}, nil)
				}()
				synctest.Wait()
				remaining := responseWriteTimeout
				if readElicitation {
					remaining /= 2
					time.Sleep(responseWriteTimeout - remaining)
					if _, err := peer.Read(t.Context()); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
				}
				time.Sleep(remaining)
				synctest.Wait()
				select {
				case written := <-answered:
					if written {
						t.Error("the response was written although the peer did not read it")
					}
				default:
					t.Error("the response deadline did not bound both the scoped write and its response")
					conn.endReading(nil)
					<-answered
				}
				if err := <-elicited; err == nil {
					t.Error("the unanswered elicitation reported success")
				}
				if err := conn.Wait(); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Wait = %v, want the response deadline", err)
				}
				synctest.Wait()
				if conn.requests.inflight != 0 {
					t.Errorf("shutdown retained %d request obligations", conn.requests.inflight)
				}
			})
		})
	}
}

type blockedElicitationStream struct {
	Connection
	entered chan struct{}
}

func (s *blockedElicitationStream) Write(ctx context.Context, message jsonrpc.Message) error {
	if request, ok := message.(*jsonrpc.Request); ok && request.Method == methodElicitationCreate {
		close(s.entered)
	}
	return s.Connection.Write(ctx, message)
}

func TestARequestAnswerCannotOvertakeItsScopedElicitationWrite(t *testing.T) {
	for name, mode := range map[string]CreateElicitationRequestValue{
		"form": &ElicitationFormMode{RequestedSchema: ElicitationSchema{
			Type: ElicitationSchemaTypeObject, Properties: map[string]ElicitationPropertySchema{},
		}},
		"url": &ElicitationURLMode{ElicitationID: "login", URL: "https://example.invalid/login"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := newTestAgentConn()
				conn.handshake.begin()
				conn.handshake.accept(PeerInfo{ClientCapabilities: ClientCapabilities{
					Elicitation: OptValue(ElicitationCapabilities{
						Form: OptValue(ElicitationFormCapabilities{}), URL: OptValue(ElicitationURLCapabilities{}),
					}),
				}})
				conn.handshake.publish(nil)
				stream := &scopedElicitationStream{
					entered: make(chan struct{}), resume: make(chan struct{}), messages: make(chan jsonrpc.Message, 2),
				}
				conn.transport = stream
				parent, err := conn.requests.accept(t.Context(), jsonrpc2.Int64ID(9))
				if err != nil {
					t.Fatal(err)
				}
				defer parent.finish()
				ctx := withServingRequest(context.WithoutCancel(parent.ctx), parent, methodAuthenticate)
				elicited := make(chan error, 1)
				go func() {
					_, err := conn.CreateElicitation(ctx, &CreateElicitationParams{Message: "?", Mode: mode})
					elicited <- err
				}()
				select {
				case <-stream.entered:
				case err := <-elicited:
					t.Fatalf("elicitation returned before writing: %v", err)
				}
				answered := make(chan bool, 1)
				go func() { answered <- conn.respond(parent, &AuthenticateResponse{}, nil) }()
				synctest.Wait()
				if len(stream.messages) != 0 {
					t.Error("the parent response overtook its request-scoped elicitation")
				}
				close(stream.resume)
				if !<-answered {
					t.Error("the parent response failed")
				}
				for range 2 {
					if request, ok := (<-stream.messages).(*jsonrpc.Request); ok {
						conn.calls.deliver(&jsonrpc.Response{ID: request.ID, Result: json.RawMessage(`{"action":"decline"}`)})
					}
				}
				if err := <-elicited; err != nil {
					t.Fatal(err)
				}
				if _, err := conn.CreateElicitation(ctx, &CreateElicitationParams{Message: "?", Mode: mode}); err == nil {
					t.Error("an answered request admitted another scoped elicitation")
				}
			})
		})
	}
}

type scopedElicitationStream struct {
	entered  chan struct{}
	resume   chan struct{}
	messages chan jsonrpc.Message
}

func (*scopedElicitationStream) Read(ctx context.Context) (jsonrpc.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *scopedElicitationStream) Write(ctx context.Context, message jsonrpc.Message) error {
	if _, request := message.(*jsonrpc.Request); request {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.messages <- message
	return nil
}

func (*scopedElicitationStream) Close() error { return nil }

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

func TestAFormHandlerCannotRewriteTheSchemaUsedToValidateItsAnswer(t *testing.T) {
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{Form: func(
			_ context.Context, _ *CreateElicitationRequest, mode *ElicitationFormMode,
		) (*CreateElicitationResponse, error) {
			mode.RequestedSchema.Required = OptValue([]string{})
			return &CreateElicitationResponse{Value: &ElicitationAcceptAction{
				Content: OptValue(map[string]ElicitationContentValue{}),
			}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := connectRawClient(t, client)
	params := `{"message":"?","mode":"form","sessionId":"session",` +
		`"requestedSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`
	if err := stream.Write(t.Context(), rawCall(7, methodElicitationCreate, params)); err != nil {
		t.Fatal(err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInternalError {
		t.Errorf("invalid answer returned %v, want internal error", code)
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

func TestAURIErrorKeepsItsGeneratedPropertyPath(t *testing.T) {
	var decoded ElicitationURLMode
	err := json.Unmarshal([]byte(`{"elicitationId":"e-1","url":"/sign-in","sessionId":"s-1"}`), &decoded)
	assertPathError(t, err, "/url")

	_, err = json.Marshal(ElicitationURLMode{
		ElicitationID: "e-1",
		URL:           "/sign-in",
		Value: &ElicitationURLModeSession{ElicitationSessionScope: ElicitationSessionScope{
			SessionID: "s-1",
		}},
	})
	assertPathError(t, err, "/url")
}

func assertPathError(t *testing.T, err error, want string) {
	t.Helper()
	var pathErr *wire.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %v is not a PathError", err)
	}
	if pathErr.Path != want {
		t.Fatalf("path = %q, want %q", pathErr.Path, want)
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

// "Clients MUST ignore notifications referencing unknown or already-completed
// IDs", which only a peer outside this package can test: an agent built here
// refuses to complete anything it did not open.
//
// Ignoring is the connection's job rather than the application's. A handler
// cannot tell a completion for a page it never showed from one it has already
// closed, because it does not hold the set.
func TestACompletionForAnUnknownElicitationIsIgnored(t *testing.T) {
	completions := make(chan ElicitationID, 4)
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{
			URL: func(
				context.Context,
				*CreateElicitationRequest,
				*ElicitationURLMode,
			) (*CreateElicitationResponse, error) {
				return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
			},
			Complete: func(_ context.Context, notification *CompleteElicitationNotification) {
				completions <- notification.ElicitationID
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

	// Never opened.
	if err := stream.Write(ctx, rawNotification(methodElicitationComplete,
		`{"elicitationId":"never-shown"}`)); err != nil {
		t.Fatalf("write the unknown completion: %v", err)
	}

	// Opened, then completed twice. The first reaches the application; the second
	// names an identifier that is no longer outstanding.
	url := `{"message":"sign in","mode":"url","elicitationId":"e-1",` +
		`"url":"https://example.invalid/","sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(7, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write the elicitation: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the elicitation answer: %v", err)
	}
	for range 2 {
		if err := stream.Write(ctx, rawNotification(methodElicitationComplete,
			`{"elicitationId":"e-1"}`)); err != nil {
			t.Fatalf("write the completion: %v", err)
		}
	}

	// One round trip after the notifications, which the ordered delivery stage
	// puts behind them, so everything above has been served by the time it answers.
	if err := stream.Write(ctx, rawCall(8, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write the second elicitation: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the second answer: %v", err)
	}

	if got := len(completions); got != 1 {
		t.Fatalf("the application saw %d completions, want 1: the unknown one and the repeat "+
			"are both the connection's to ignore", got)
	}
	if id := <-completions; id != "e-1" {
		t.Errorf("the application saw a completion for %q", id)
	}
}

func TestAReusedOutstandingURLIDIsRefusedBeforeTheHandler(t *testing.T) {
	var served int
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{
			URL: func(
				context.Context,
				*CreateElicitationRequest,
				*ElicitationURLMode,
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

	url := `{"message":"sign in","mode":"url","elicitationId":"same",` +
		`"url":"https://example.invalid/","sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(7, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write first elicitation: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read first answer: %v", err)
	}
	if err := stream.Write(ctx, rawCall(8, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write duplicate elicitation: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
		t.Errorf("the duplicate was answered %v, want invalid params", code)
	}
	if served != 1 {
		t.Errorf("the duplicate reached the URL handler; it ran %d times", served)
	}
}

func TestAnUnencodableURLAcceptDoesNotKeepItsID(t *testing.T) {
	var served int
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{
			URL: func(
				context.Context,
				*CreateElicitationRequest,
				*ElicitationURLMode,
			) (*CreateElicitationResponse, error) {
				served++
				if served == 1 {
					var value *ElicitationContentValueString
					return &CreateElicitationResponse{Value: &ElicitationAcceptAction{
						Content: OptValue(map[string]ElicitationContentValue{"broken": value}),
					}}, nil
				}
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

	url := `{"message":"sign in","mode":"url","elicitationId":"same",` +
		`"url":"https://example.invalid/","sessionId":"sess-1"}`
	if writeErr := stream.Write(ctx, rawCall(7, methodElicitationCreate, url)); writeErr != nil {
		t.Fatalf("write malformed answer's request: %v", writeErr)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInternalError {
		t.Fatalf("the malformed handler answer returned %v, want internal error", code)
	}
	if writeErr := stream.Write(ctx, rawCall(8, methodElicitationCreate, url)); writeErr != nil {
		t.Fatalf("write retry: %v", writeErr)
	}
	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatalf("read retry: %v", err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok || response.Error != nil {
		t.Fatalf("retry returned %#v, want a successful response", message)
	}
	if served != 2 {
		t.Errorf("the released ID reached the handler %d times, want 2", served)
	}
}

// A client that accepts URL interactions and does not want to hear they finished
// still tracks them, because the tracking is what the ignore rule is made of.
func TestACompletionIsClosedEvenWithNoHandler(t *testing.T) {
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
		Elicitation: &ElicitationHandlers{
			URL: func(
				context.Context,
				*CreateElicitationRequest,
				*ElicitationURLMode,
			) (*CreateElicitationResponse, error) {
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

	url := `{"message":"sign in","mode":"url","elicitationId":"e-1",` +
		`"url":"https://example.invalid/","sessionId":"sess-1"}`
	if err := stream.Write(ctx, rawCall(7, methodElicitationCreate, url)); err != nil {
		t.Fatalf("write the elicitation: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if !conn.elicitations.receiveCompletion("e-1") {
		t.Fatal("the elicitation was not recorded, so a completion for it would have been ignored")
	}
}
