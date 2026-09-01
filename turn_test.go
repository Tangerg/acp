package acp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// A turn's lifetime, and the three things that used to be confused with it: the
// request that carries it, the caller waiting on it, and the handler serving it.

// A permission request that arrives after a cancellation has begun is answered
// cancelled, and the user never sees it.
//
// The registration said so and the connection did not do it. The request was not
// yet on record when registration tried to claim the right to answer it, so the
// claim failed, registration returned as though nothing had happened, and the
// dialog opened for a turn the user had already cancelled.
func TestAPermissionRequestArrivingAfterCancellationNeverReachesTheUser(t *testing.T) {
	asked := make(chan struct{}, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			asked <- struct{}{}
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: "yes"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	conn, stream, ctx := rawAgentFor(t, client)
	session := openSession(ctx, t, conn, stream)

	// A turn, then a cancellation of it. Cancel is synchronous about the state it
	// establishes, so what follows is not a race: the session is cancelling before
	// the permission request is written.
	prompted := make(chan error, 1)
	go func() {
		_, err := session.Prompt(context.Background(), &acp.PromptParams{})
		prompted <- err
	}()
	prompt := expectCall(ctx, t, stream, "session/prompt")

	cancelled := make(chan error, 1)
	go func() { cancelled <- session.Cancel(context.Background(), nil) }()
	expectNotification(ctx, t, stream, "session/cancel")
	if err := <-cancelled; err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	answer := roundTrip(ctx, t, stream, 77, "session/request_permission",
		`{"sessionId":"sess-1","toolCall":{"toolCallId":"call-1"},`+
			`"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"}]}`)
	if answer.Error != nil {
		t.Fatalf("the permission request was refused: %v", answer.Error)
	}
	if got := string(answer.Result); !strings.Contains(got, `"cancelled"`) {
		t.Fatalf("the permission request was answered %s, want the cancelled outcome", got)
	}
	select {
	case <-asked:
		t.Fatal("the RequestPermission handler ran for a turn the user had already cancelled, so a " +
			"dialog opened whose answer was going to be thrown away")
	default:
	}

	// And the turn still ends the way the protocol says it does.
	answerCall(ctx, t, stream, prompt, `{"stopReason":"cancelled"}`)
	if err := <-prompted; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

func TestAPermissionRequestOutsideAPromptIsDispatched(t *testing.T) {
	asked := make(chan struct{}, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			asked <- struct{}{}
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: "yes"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	conn, stream, ctx := rawAgentFor(t, client)
	openSession(ctx, t, conn, stream)
	answer := roundTrip(ctx, t, stream, 76, "session/request_permission",
		`{"sessionId":"sess-1","toolCall":{"toolCallId":"call-0"},`+
			`"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"}]}`)
	if answer.Error != nil {
		t.Fatalf("permission outside a prompt was refused: %v", answer.Error)
	}
	if got := string(answer.Result); !strings.Contains(got, `"selected"`) {
		t.Fatalf("permission outside a prompt was answered %s, want the handler's selection", got)
	}
	select {
	case <-asked:
	default:
		t.Fatal("a valid permission request outside a prompt never reached the user")
	}
}

// A prompt that is refused does not keep the session it named.
//
// The turn is claimed during ordered admission, before the connection knows whether the
// request can be served at all. Releasing it only in the handler's own defer left
// every rejection path holding the session for the life of the connection, and
// every later prompt for that session refused as concurrent.
func TestARejectedPromptLeavesItsSessionFree(t *testing.T) {
	tests := map[string]struct {
		initializeFirst bool
		params          string
		says            string
	}{
		"before initialize": {
			params: `{"sessionId":"sess-1","prompt":[]}`,
			says:   "before initialize",
		},
		"with parameters that do not decode": {
			initializeFirst: true,
			// A decodable sessionId, so the turn is claimed before dispatch, and a
			// prompt that is not an array, so the handler's own decode fails.
			params: `{"sessionId":"sess-1","prompt":"not an array"}`,
			says:   "prompt",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			prompted := make(chan struct{}, 1)
			agent := testAgent(t, func(
				context.Context, *acp.AgentSession, *acp.PromptRequest,
			) (*acp.PromptResponse, error) {
				prompted <- struct{}{}
				return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			})
			_, stream, ctx := rawClientFor(t, agent)
			if test.initializeFirst {
				initializeRaw(ctx, t, stream)
			}

			rejected := roundTrip(ctx, t, stream, 10, "session/prompt", test.params)
			if rejected.Error == nil {
				t.Fatal("the prompt was accepted, so this test proves nothing about rejection")
			}
			if !strings.Contains(rejected.Error.Error(), test.says) {
				t.Errorf("rejected with %v, which is not the rejection this test meant", rejected.Error)
			}

			// The same session, with nothing running in it, must be usable.
			if !test.initializeFirst {
				initializeRaw(ctx, t, stream)
			}
			accepted := roundTrip(ctx, t, stream, 11, "session/prompt", `{"sessionId":"sess-1","prompt":[]}`)
			if accepted.Error != nil {
				t.Fatalf("the next prompt for the same session was refused with %v; the rejected one "+
					"is still holding the session", accepted.Error)
			}
			select {
			case <-prompted:
			default:
				t.Fatal("the accepted prompt never reached the handler")
			}
		})
	}
}

// A caller that stops waiting has not ended the turn.
//
// Prompt used to clear the session the moment it returned, whatever the reason.
// Cancelling its context therefore announced a fact the protocol had not
// established: the agent was still working, still owed an answer, and a second
// prompt could start underneath the first.
func TestACancelledPromptCallerDoesNotEndTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan struct{}, 1)
		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate: func(context.Context, *acp.SessionNotification) {},
			RequestPermission: func(
				context.Context,
				*acp.RequestPermissionRequest,
			) (*acp.RequestPermissionResponse, error) {
				asked <- struct{}{}
				return &acp.RequestPermissionResponse{
					Outcome: &acp.SelectedPermissionOutcome{OptionID: "yes"},
				}, nil
			},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		conn, stream, ctx := rawAgentFor(t, client)
		session := openSession(ctx, t, conn, stream)

		abandoned, giveUp := context.WithCancel(context.Background())
		failed := make(chan error, 1)
		go func() {
			_, err := session.Prompt(abandoned, &acp.PromptParams{})
			failed <- err
		}()
		prompt := expectCall(ctx, t, stream, "session/prompt")

		giveUp()
		if err := <-failed; !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt returned %v, want context.Canceled", err)
		}
		// The peer is told, as a courtesy on a budget of its own.
		expectNotification(ctx, t, stream, "$/cancel_request")

		// The turn is still the agent's to finish, so the session is not free.
		if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); !errors.Is(
			err, acp.ErrPromptInProgress,
		) {
			t.Fatalf("a second prompt returned %v, want ErrPromptInProgress: the first turn is still "+
				"running and session/cancel names no turn", err)
		}

		// The documented sequence: give up waiting, then end the turn. The
		// cancellation must still find a turn to attach to.
		cancelled := make(chan error, 1)
		go func() { cancelled <- session.Cancel(context.Background(), nil) }()
		expectNotification(ctx, t, stream, "session/cancel")
		if err := <-cancelled; err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		synctest.Wait()
		if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); !errors.Is(
			err, acp.ErrPromptInProgress,
		) {
			t.Fatalf("a prompt was accepted after Cancel and before the agent answered: %v", err)
		}

		// The turn ends where the protocol says it ends.
		answerCall(ctx, t, stream, prompt, `{"stopReason":"cancelled"}`)
		synctest.Wait()

		// And now the session belongs to whatever comes next. The new turn's
		// permission requests are the user's again, which they would not be if the
		// cancelled turn's state were still standing.
		next := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), &acp.PromptParams{})
			next <- err
		}()
		second := expectCall(ctx, t, stream, "session/prompt")

		answer := roundTrip(ctx, t, stream, 78, "session/request_permission",
			`{"sessionId":"sess-1","toolCall":{"toolCallId":"call-2"},`+
				`"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"}]}`)
		if answer.Error != nil {
			t.Fatalf("the new turn's permission request was refused: %v", answer.Error)
		}
		if got := string(answer.Result); !strings.Contains(got, `"selected"`) {
			t.Fatalf("the new turn's permission request was answered %s; the cancelled turn's state "+
				"is still standing", got)
		}
		select {
		case <-asked:
		default:
			t.Fatal("the new turn's permission request never reached the user")
		}

		answerCall(ctx, t, stream, second, `{"stopReason":"end_turn"}`)
		if err := <-next; err != nil {
			t.Fatalf("the next prompt: %v", err)
		}
	})
}

type observingTransport struct {
	acp.Transport
	method string
	began  chan<- struct{}
}

func (t *observingTransport) Connect(ctx context.Context) (acp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &observingConnection{Connection: connection, method: t.method, began: t.began}, nil
}

type observingConnection struct {
	acp.Connection
	method string
	began  chan<- struct{}
}

// A permission answer that has been claimed but is still blocked in Write is
// still pending from the agent's point of view. This wrapper makes that interval
// deterministic and records if session/cancel attempts to overtake it.
type orderedCancellationTransport struct {
	acp.Transport
	responseBegan chan<- struct{}
	release       <-chan struct{}
	cancelBegan   chan<- struct{}
}

func (t *orderedCancellationTransport) Connect(ctx context.Context) (acp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &orderedCancellationConnection{
		Connection:    connection,
		responseBegan: t.responseBegan,
		release:       t.release,
		cancelBegan:   t.cancelBegan,
	}, nil
}

type orderedCancellationConnection struct {
	acp.Connection
	responseBegan chan<- struct{}
	release       <-chan struct{}
	cancelBegan   chan<- struct{}
}

func (c *orderedCancellationConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	switch message := message.(type) {
	case *jsonrpc.Response:
		c.responseBegan <- struct{}{}
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	case *jsonrpc.Request:
		if message.Method == "session/cancel" {
			c.cancelBegan <- struct{}{}
		}
	}
	return c.Connection.Write(ctx, message)
}

func (c *observingConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if request, ok := message.(*jsonrpc.Request); ok && request.Method == c.method {
		select {
		case c.began <- struct{}{}:
		default:
		}
	}
	return c.Connection.Write(ctx, message)
}

// openSession runs session/new against a hand-driven agent and returns the handle
// the client built from the answer.
func openSession(
	ctx context.Context,
	t *testing.T,
	conn *acp.ClientConn,
	stream acp.Connection,
) *acp.ClientSession {
	t.Helper()

	type opened struct {
		session *acp.ClientSession
		err     error
	}
	done := make(chan opened, 1)
	go func() {
		session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{
			Cwd:        "/w",
			McpServers: []acp.McpServer{},
		})
		done <- opened{session, err}
	}()

	request := expectCall(ctx, t, stream, "session/new")
	answerCall(ctx, t, stream, request, `{"sessionId":"sess-1"}`)

	result := <-done
	if result.err != nil {
		t.Fatalf("NewSession: %v", result.err)
	}
	return result.session
}

// expectCall reads one message and insists it is a call of the method named.
func expectCall(ctx context.Context, t *testing.T, stream acp.Connection, method string) *jsonrpc.Request {
	t.Helper()

	message := readRaw(ctx, t, stream)
	request, ok := message.(*jsonrpc.Request)
	if !ok || !request.IsCall() || request.Method != method {
		t.Fatalf("read %#v, want a call of %s", message, method)
	}
	return request
}

func expectNotification(ctx context.Context, t *testing.T, stream acp.Connection, method string) {
	t.Helper()

	message := readRaw(ctx, t, stream)
	request, ok := message.(*jsonrpc.Request)
	if !ok || request.IsCall() || request.Method != method {
		t.Fatalf("read %#v, want a notification of %s", message, method)
	}
}

func answerCall(ctx context.Context, t *testing.T, stream acp.Connection, request *jsonrpc.Request, result string) {
	t.Helper()

	writeRaw(ctx, t, stream, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, idOf(t, request), result))
}

func TestCancellationWaitsForAClaimedPermissionAnswerToReachTheWire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate: func(context.Context, *acp.SessionNotification) {},
			RequestPermission: func(
				context.Context,
				*acp.RequestPermissionRequest,
			) (*acp.RequestPermissionResponse, error) {
				return &acp.RequestPermissionResponse{
					Outcome: &acp.SelectedPermissionOutcome{OptionID: "yes"},
				}, nil
			},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		clientSide, agentSide := acp.NewInMemoryTransports()
		responseBegan := make(chan struct{}, 1)
		release := make(chan struct{})
		cancelBegan := make(chan struct{}, 1)
		transport := &orderedCancellationTransport{
			Transport:     clientSide,
			responseBegan: responseBegan,
			release:       release,
			cancelBegan:   cancelBegan,
		}
		ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		stream, err := agentSide.Connect(ctx)
		if err != nil {
			t.Fatalf("connect raw agent: %v", err)
		}
		defer stream.Close() //nolint:errcheck // test cleanup.

		connected := make(chan connectResult, 1)
		go func() {
			conn, err := client.Connect(ctx, transport)
			connected <- connectResult{conn: conn, err: err}
		}()
		if err := answerHandshake(ctx, stream); err != nil {
			t.Fatalf("answer handshake: %v", err)
		}
		result := <-connected
		if result.err != nil {
			t.Fatalf("Client.Connect: %v", result.err)
		}
		conn := result.conn
		defer conn.Close() //nolint:errcheck // test cleanup.
		session := openSession(ctx, t, conn, stream)

		prompted := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), &acp.PromptParams{})
			prompted <- err
		}()
		prompt := expectCall(ctx, t, stream, "session/prompt")

		writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","id":78,"method":"session/request_permission",`+
			`"params":{"sessionId":"sess-1","toolCall":{"toolCallId":"call-2"},`+
			`"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"}]}}`)
		<-responseBegan

		cancelled := make(chan error, 1)
		go func() { cancelled <- session.Cancel(context.Background(), nil) }()
		synctest.Wait()
		select {
		case <-cancelBegan:
			t.Fatal("session/cancel overtook a permission answer whose write had not settled")
		default:
		}

		close(release)
		permission := readRaw(ctx, t, stream)
		if response, ok := permission.(*jsonrpc.Response); !ok || response.Error != nil {
			t.Fatalf("read %#v, want the selected permission response", permission)
		}
		expectNotification(ctx, t, stream, "session/cancel")
		if err := <-cancelled; err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		answerCall(ctx, t, stream, prompt, `{"stopReason":"cancelled"}`)
		if err := <-prompted; err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
}

// A cancellation holds its turn until the notification is on the wire.
//
// session/cancel names only a session, so which turn it cancels is decided by
// when it arrives. Releasing the session as soon as the old prompt was answered
// let the next turn start first, and the notification the caller had already
// asked for then cancelled it instead.
func TestACancellationThatHasNotBeenSentStillHoldsItsTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
			RequestPermission: denyingPermission,
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		clientSide, agentSide := acp.NewInMemoryTransports()
		startedCancel := make(chan struct{}, 1)
		observed := &observingTransport{
			Transport: clientSide,
			method:    "session/cancel",
			began:     startedCancel,
		}
		ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		stream, err := agentSide.Connect(ctx)
		if err != nil {
			t.Fatalf("connect raw agent: %v", err)
		}
		defer stream.Close() //nolint:errcheck // test cleanup.
		connected := make(chan connectResult, 1)
		go func() {
			conn, err := client.Connect(ctx, observed)
			connected <- connectResult{conn: conn, err: err}
		}()
		if err := answerHandshake(ctx, stream); err != nil {
			t.Fatalf("answer handshake: %v", err)
		}
		result := <-connected
		if result.err != nil {
			t.Fatalf("Client.Connect: %v", result.err)
		}
		conn := result.conn
		defer conn.Close() //nolint:errcheck // test cleanup.
		session := openSession(ctx, t, conn, stream)

		prompted := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), &acp.PromptParams{})
			prompted <- err
		}()
		first := expectCall(ctx, t, stream, "session/prompt")

		// Cancel starts and blocks writing its notification, because nothing is
		// reading this end yet. That is the interval the test is about.
		cancelled := make(chan error, 1)
		go func() { cancelled <- session.Cancel(context.Background(), nil) }()
		<-startedCancel

		// The agent answers the old turn first, which is exactly the ordering that
		// used to reopen the session.
		answerCall(ctx, t, stream, first, `{"stopReason":"cancelled"}`)
		if err := <-prompted; err != nil {
			t.Fatalf("Prompt: %v", err)
		}

		// A deadline rather than nothing, so that a prompt wrongly admitted fails
		// here instead of blocking on a write nobody is reading.
		refused, giveUp := context.WithTimeout(context.Background(), time.Second)
		defer giveUp()
		if _, err := session.Prompt(refused, &acp.PromptParams{}); !errors.Is(
			err, acp.ErrPromptInProgress,
		) {
			t.Fatalf("a prompt was accepted while a cancellation for the previous turn had not been "+
				"sent, so the agent would have applied that cancellation to this turn: %v", err)
		}

		// Once the notification is out, the session is the next turn's.
		expectNotification(ctx, t, stream, "session/cancel")
		if err := <-cancelled; err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		next := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), &acp.PromptParams{})
			next <- err
		}()
		second := expectCall(ctx, t, stream, "session/prompt")
		answerCall(ctx, t, stream, second, `{"stopReason":"end_turn"}`)
		if err := <-next; err != nil {
			t.Fatalf("the next prompt: %v", err)
		}
	})
}
