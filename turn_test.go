package acp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

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
	session := openSession(ctx, t, conn, stream, "sess-1")

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

// A prompt that is refused does not keep the session it named.
//
// The turn is claimed on the read loop, before the connection knows whether the
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
			// A decodable sessionId, so the turn is claimed on the read loop, and a
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
		session := openSession(ctx, t, conn, stream, "sess-1")

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

// openSession runs session/new against a hand-driven agent and returns the handle
// the client built from the answer.
func openSession(
	ctx context.Context,
	t *testing.T,
	conn *acp.ClientConn,
	stream acp.Connection,
	id acp.SessionID,
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
	answerCall(ctx, t, stream, request, fmt.Sprintf(`{"sessionId":%q}`, id))

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
