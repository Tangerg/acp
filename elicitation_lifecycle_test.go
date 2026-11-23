package acp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// A URL elicitation is the one exchange that can outlive its own response: an
// accept consents to start the out-of-band interaction, and the agent says later
// that the accepted interaction is finished.
func TestAURLElicitationIsFinishedByItsCompletion(t *testing.T) {
	accepted := make(chan acp.ElicitationID, 1)
	finished := make(chan acp.ElicitationID, 1)

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				_ context.Context, _ *acp.ClientConn, _ *acp.CreateElicitationRequest,
				mode *acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				accepted <- mode.ElicitationID
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{},
				}, nil
			},
			Complete: func(_ context.Context, _ *acp.ClientConn, notification *acp.CompleteElicitationNotification) {
				finished <- notification.ElicitationID
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var elicitErr, completeErr error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, elicitErr = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "sign in",
			Mode: &acp.ElicitationURLMode{
				ElicitationID: "elicit-1",
				URL:           "https://example.invalid/sign-in",
			},
		})
		if elicitErr == nil {
			completeErr = session.Conn().CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
				ElicitationID: "elicit-1",
			})
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if elicitErr != nil {
		t.Fatalf("CreateElicitation: %v", elicitErr)
	}
	if completeErr != nil {
		t.Fatalf("CompleteElicitation: %v", completeErr)
	}
	if id := <-accepted; id != "elicit-1" {
		t.Errorf("the client accepted %q, want elicit-1", id)
	}
	if done := <-finished; done != "elicit-1" {
		t.Errorf("the client was told %q finished, want elicit-1", done)
	}
}

func TestOnlyAnAcceptedURLElicitationBecomesOutstanding(t *testing.T) {
	var handled int
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				handled++
				var action acp.CreateElicitationResponseValue
				switch handled {
				case 1:
					action = &acp.CreateElicitationResponseDecline{}
				case 2:
					action = &acp.CreateElicitationResponseCancel{}
				default:
					action = &acp.ElicitationAcceptAction{}
				}
				return &acp.CreateElicitationResponse{Value: action}, nil
			},
			Complete: func(context.Context, *acp.ClientConn, *acp.CompleteElicitationNotification) {},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var createErrors, completionErrors []error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		params := &acp.CreateElicitationParams{
			Message: "sign in",
			Mode: &acp.ElicitationURLMode{
				ElicitationID: "reusable",
				URL:           "https://example.invalid/sign-in",
			},
		}
		for range 3 {
			_, createErr := session.CreateElicitation(ctx, params)
			createErrors = append(createErrors, createErr)
			completionCtx := ctx
			if len(createErrors) == 3 {
				var cancel context.CancelFunc
				completionCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			completionErrors = append(completionErrors, session.Conn().CompleteElicitation(
				completionCtx,
				&acp.CompleteElicitationNotification{ElicitationID: "reusable"},
			))
		}
		completionErrors = append(completionErrors, session.Conn().CompleteElicitation(
			ctx,
			&acp.CompleteElicitationNotification{ElicitationID: "reusable"},
		))
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for i, err := range createErrors {
		if err != nil {
			t.Errorf("CreateElicitation %d: %v", i+1, err)
		}
	}
	if completionErrors[0] == nil || completionErrors[1] == nil {
		t.Error("a declined or cancelled URL elicitation was treated as outstanding")
	}
	if !errors.Is(completionErrors[2], context.Canceled) {
		t.Errorf("the unsent completion returned %v, want context cancellation", completionErrors[2])
	}
	if completionErrors[3] != nil {
		t.Errorf("the accepted URL elicitation could not be retried: %v", completionErrors[3])
	}
}

// The identifier's lifetime, which the connection owns because neither side's
// application can see the whole set.
//
// An agent "MUST keep each elicitationId unique among outstanding URL
// elicitations on that Agent-Client connection", and a URL elicitation stays
// outstanding past its own response — an accept consents to start it and the
// completion says that accepted interaction is finished.
func TestAURLElicitationIdentifierIsUniqueWhileItIsOutstanding(t *testing.T) {
	accepted := make(chan struct{}, 4)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				accepted <- struct{}{}
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
			Complete: func(context.Context, *acp.ClientConn, *acp.CompleteElicitationNotification) {},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	page := func(id acp.ElicitationID) *acp.CreateElicitationParams {
		return &acp.CreateElicitationParams{
			Message: "sign in",
			Mode: &acp.ElicitationURLMode{
				ElicitationID: id,
				URL:           "https://example.invalid/",
			},
		}
	}

	var reused, unopened, afterCompleting, secondCompletion error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		conn := session.Conn()
		if _, err := session.CreateElicitation(ctx, page("e-1")); err != nil {
			return nil, err
		}
		_, reused = session.CreateElicitation(ctx, page("e-1"))
		unopened = conn.CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
			ElicitationID: "never-opened",
		})

		// Completing frees the name, so the same page may be opened again.
		if err := conn.CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
			ElicitationID: "e-1",
		}); err != nil {
			return nil, err
		}
		secondCompletion = conn.CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
			ElicitationID: "e-1",
		})
		_, afterCompleting = session.CreateElicitation(ctx, page("e-1"))
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if reused == nil || !strings.Contains(reused.Error(), "already in use") {
		t.Errorf("a second elicitation under a live identifier was answered %v, want a refusal", reused)
	}
	if unopened == nil || !strings.Contains(unopened.Error(), "not outstanding") {
		t.Errorf("completing an elicitation nothing opened was answered %v, want a refusal", unopened)
	}
	if secondCompletion == nil {
		t.Error("completing the same elicitation twice was allowed, so the second names nothing")
	}
	if afterCompleting != nil {
		t.Errorf("the identifier was not free again after its completion: %v", afterCompleting)
	}
	if len(accepted) != 2 {
		t.Errorf("the client accepted %d URL interactions, want 2: the first and the reuse after completion", len(accepted))
	}
}

// The bound on outstanding URL elicitations comes from the connection's own
// Limits. Reaching it forgets the oldest accepted interaction rather than
// refusing the next or ending the connection, because completing one is
// optional and a forgotten identifier only loses its late completion.
//
// Driven through the public API on purpose. The reservation reads the bound from
// the connection that resolved it, and a test that supplied its own would pass
// with that wiring removed.
func TestTheOutstandingURLElicitationBoundIsTheConfiguredOne(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var first, second error
	agent, err := acp.NewAgent(&acp.AgentConfig{
		Limits:     acp.Limits{OutstandingElicitations: 1},
		NewSession: openTestSession,
		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			elicit := func(id acp.ElicitationID) error {
				_, refused := session.CreateElicitation(ctx, &acp.CreateElicitationParams{
					Message: "sign in",
					Mode: &acp.ElicitationURLMode{
						ElicitationID: id,
						URL:           "https://example.invalid/sign-in",
					},
				})
				return refused
			}
			first = elicit("elicit-1")
			second = elicit("elicit-2")
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if first != nil {
		t.Fatalf("the first elicitation was refused within the bound: %v", first)
	}
	// Completing an accepted interaction is optional, so the bound forgets the
	// oldest outstanding one rather than refusing the next.
	if second != nil {
		t.Fatalf("the elicitation past the bound was refused: %v", second)
	}

	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("the connection did not survive reaching the bound: %v", err)
	}
}
