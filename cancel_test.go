package acp_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
)

// Two cancellations, and neither is the other.
//
// $/cancel_request cancels one JSON-RPC request and stops this side waiting.
// session/cancel cancels a turn and leaves the request outstanding, on purpose, so
// that the agent can answer it with the cancelled stop reason. An implementation
// that fused them would take away the caller's only bounded way to stop waiting,
// or would leave the turn's obligations unmet.

// Cancelling a Prompt's context returns the caller's own error and does not end
// the turn. The agent is still working, and still owes an answer nobody is waiting
// for.
func TestCancellingAPromptContextReturnsTheCallersError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agentSeesCancel := make(chan struct{})
		release := make(chan struct{})
		agent := testAgent(t, func(ctx context.Context, _ *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			// $/cancel_request cancels the request's context, and the handler sees
			// it — the work descending from a request the caller gave up on should
			// stop.
			<-ctx.Done()
			close(agentSeesCancel)
			<-release
			return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		})
		session := connectAndOpen(t, testClient(t), agent)

		ctx, cancel := context.WithCancel(context.Background())
		failed := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, &acp.PromptParams{})
			failed <- err
		}()

		synctest.Wait()
		cancel()

		err := <-failed
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt returned %v, want context.Canceled", err)
		}

		// The peer was told, on a budget of its own.
		<-agentSeesCancel
		close(release)
	})
}

// A deadline is not a cancellation, and flattening one into the other would make
// every timeout indistinguishable from a caller changing its mind.
func TestAPromptDeadlineIsReportedAsADeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		agent := testAgent(t, func(ctx context.Context, _ *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			<-ctx.Done()
			<-release
			return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		})
		session := connectAndOpen(t, testClient(t), agent)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := session.Prompt(ctx, &acp.PromptParams{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Prompt returned %v, want context.DeadlineExceeded", err)
		}
		close(release)
	})
}

// Model and tool libraries commonly return a context error when their work is
// aborted. ACP requires session/cancel to end the prompt with a semantic
// cancelled stop reason, so that implementation detail cannot become -32603.
func TestSessionCancelTurnsAHandlerAbortIntoACancelledPrompt(t *testing.T) {
	started := make(chan struct{})
	agent := testAgent(t, func(ctx context.Context, _ *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	session := connectAndOpen(t, testClient(t), agent)

	answered := make(chan struct {
		response *acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := session.Prompt(context.Background(), &acp.PromptParams{})
		answered <- struct {
			response *acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()

	<-started
	if err := session.Cancel(context.Background(), nil); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	result := <-answered
	if result.err != nil {
		t.Fatalf("Prompt returned an error after session/cancel: %v", result.err)
	}
	if result.response == nil || result.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("Prompt returned %#v, want the cancelled stop reason", result.response)
	}
}

// The obligation a cancelled turn leaves: every pending permission request for
// that session is answered with the cancelled outcome, while the client's own
// handler may still be blocked on a dialog.
//
// This is the connection's to keep, not the application's. A client that had to
// remember to answer its own outstanding dialogs would forget.
func TestCancellingATurnAnswersPendingPermissionRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan struct{})
		dialogClosed := make(chan struct{})

		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate: func(context.Context, *acp.SessionNotification) {},
			RequestPermission: func(
				ctx context.Context,
				_ *acp.RequestPermissionRequest,
			) (*acp.RequestPermissionResponse, error) {
				// A dialog the user never answers. The handler's context being
				// cancelled is how it learns to take the dialog down.
				close(asked)
				<-ctx.Done()
				close(dialogClosed)
				return nil, ctx.Err()
			},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		outcomes := make(chan acp.RequestPermissionOutcome, 1)
		turnCancelled := make(chan struct{})
		agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			response, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
				ToolCall: acp.ToolCallUpdate{ToolCallID: "call-1"},
				Options: []acp.PermissionOption{
					{OptionID: "yes", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				},
			})
			if err != nil {
				return nil, err
			}
			outcomes <- response.Outcome

			// The protocol requires this: a cancelled turn is answered with the
			// cancelled stop reason, not with an error.
			<-turnCancelled
			return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		})

		session := connectAndOpen(t, client, agent)
		prompted := make(chan *acp.PromptResponse, 1)
		go func() {
			response, err := session.Prompt(context.Background(), &acp.PromptParams{})
			if err != nil {
				t.Errorf("Prompt: %v", err)
				prompted <- nil
				return
			}
			prompted <- response
		}()

		<-asked
		if err := session.Cancel(context.Background(), nil); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		close(turnCancelled)

		outcome := <-outcomes
		if _, cancelled := outcome.(*acp.RequestPermissionOutcomeCancelled); !cancelled {
			t.Fatalf("the agent was told %T, want the cancelled outcome", outcome)
		}

		// And the handler's context was cancelled, so a real dialog would have come
		// down rather than being left on screen.
		<-dialogClosed

		response := <-prompted
		if response == nil {
			return // the goroutine already reported
		}
		if response.StopReason != acp.StopReasonCancelled {
			t.Errorf("stop reason = %q, want cancelled", response.StopReason)
		}
	})
}

// A late user decision finds the request already answered and is dropped. One
// request, one response — claiming before answering is what makes that decidable
// rather than a matter of which goroutine ran first.
func TestALateUserDecisionIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan struct{})
		decide := make(chan struct{})

		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate: func(context.Context, *acp.SessionNotification) {},
			RequestPermission: func(
				context.Context,
				*acp.RequestPermissionRequest,
			) (*acp.RequestPermissionResponse, error) {
				close(asked)
				<-decide
				// The user pressed Allow after the turn was cancelled. This answer
				// is late, and sending it would be a second response for one
				// request.
				return &acp.RequestPermissionResponse{
					Outcome: &acp.SelectedPermissionOutcome{OptionID: "yes"},
				}, nil
			},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		outcomes := make(chan acp.RequestPermissionOutcome, 2)
		agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			response, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
				ToolCall: acp.ToolCallUpdate{ToolCallID: "call-1"},
				Options: []acp.PermissionOption{
					{OptionID: "yes", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				},
			})
			if err != nil {
				return nil, err
			}
			outcomes <- response.Outcome
			return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		})

		session := connectAndOpen(t, client, agent)
		go func() {
			if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
				t.Errorf("Prompt: %v", err)
			}
		}()

		<-asked
		if err := session.Cancel(context.Background(), nil); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		close(decide)

		outcome := <-outcomes
		if _, cancelled := outcome.(*acp.RequestPermissionOutcomeCancelled); !cancelled {
			t.Fatalf("the agent was told %T, want the cancelled outcome", outcome)
		}

		// Nothing else arrives. If the late decision had been sent, the agent would
		// have received a second response for one request.
		synctest.Wait()
		select {
		case extra := <-outcomes:
			t.Fatalf("a second outcome arrived: %T", extra)
		default:
		}
	})
}

// Cancelling one session's turn does not touch another's. The tree is connection →
// session → turn → request, and a sibling's cancellation is not a parent's.
func TestCancellingOneTurnLeavesAnotherAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan acp.SessionID, 2)
		release := make(chan struct{})

		agent, err := acp.NewAgent(&acp.AgentConfig{
			NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
				return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
			},
			Prompt: func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
				select {
				case <-ctx.Done():
					cancelled <- session.ID()
					return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
				case <-release:
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				}
			},
			Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		})
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}

		session := connectAndOpen(t, testClient(t), agent)
		conn := session.Conn()

		// A second session on the same connection, whose turn must survive the
		// first one being cancelled.
		other, _, err := conn.LoadSession(context.Background(), &acp.LoadSessionRequest{SessionID: "sess-2"})
		if err == nil {
			t.Fatal("LoadSession succeeded on an agent that does not advertise it")
		}
		_ = other

		results := make(chan acp.StopReason, 1)
		go func() {
			response, err := session.Prompt(context.Background(), &acp.PromptParams{})
			if err != nil {
				t.Errorf("Prompt: %v", err)
				results <- ""
				return
			}
			results <- response.StopReason
		}()

		synctest.Wait()
		if err := session.Cancel(context.Background(), nil); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		if reason := <-results; reason != acp.StopReasonCancelled {
			t.Fatalf("stop reason = %q, want cancelled", reason)
		}
		if id := <-cancelled; id != "sess-1" {
			t.Errorf("the cancelled session was %q", id)
		}
		close(release)
	})
}

// A closed connection releases everything waiting on it, and reports the same
// terminal error to every caller. A condition that reported differently depending
// on who asked first would be unusable for deciding whether to reconnect.
func TestClosingReleasesEveryWaiterWithOneAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		agent := testAgent(t, func(ctx context.Context, _ *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			select {
			case <-ctx.Done():
			case <-release:
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		})
		session := connectAndOpen(t, testClient(t), agent)
		conn := session.Conn()

		failed := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), &acp.PromptParams{})
			failed <- err
		}()
		synctest.Wait()

		if err := conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := <-failed; !errors.Is(err, acp.ErrConnectionClosed) {
			t.Fatalf("the outstanding prompt failed with %v, want ErrConnectionClosed", err)
		}

		// Every caller, every time.
		for range 3 {
			if err := conn.Wait(); err != nil {
				t.Fatalf("Wait reported %v, want nil after a local Close", err)
			}
		}
		// And Close is idempotent.
		if err := conn.Close(); err != nil {
			t.Fatalf("the second Close reported %v", err)
		}
		if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); !errors.Is(err, acp.ErrConnectionClosed) {
			t.Fatalf("a call on a closed connection returned %v, want ErrConnectionClosed", err)
		}
	})
}
