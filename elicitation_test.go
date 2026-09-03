package acp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// Elicitation, driven from an agent's prompt handler for the session scope and
// from its authenticate handler for the request scope, because those are the two
// places the schema says an elicitation comes from.

func formSchema() acp.ElicitationSchema {
	return acp.ElicitationSchema{
		Type: acp.ElicitationSchemaTypeObject,
		Properties: map[string]acp.ElicitationPropertySchema{
			"branch": &acp.StringPropertySchema{},
		},
	}
}

// A form elicitation crosses the connection, reaches the mode's handler, and the
// user's answer comes back to the agent.
func TestAFormElicitationReachesTheUserAndAnswers(t *testing.T) {
	var sawMessage string
	var sawScope acp.ElicitationFormModeValue

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context,
				request *acp.CreateElicitationRequest,
				mode *acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				sawMessage = request.Message
				sawScope = mode.Value
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{
						Content: acp.OptValue(map[string]acp.ElicitationContentValue{
							"branch": stringContent("main"),
						}),
					},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var answer *acp.CreateElicitationResponse
	var elicitErr error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		answer, elicitErr = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message:    "which branch?",
			Mode:       &acp.ElicitationFormMode{RequestedSchema: formSchema()},
			ToolCallID: acp.OptValue(acp.ToolCallID("call-1")),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if elicitErr != nil {
		t.Fatalf("CreateElicitation: %v", elicitErr)
	}
	if sawMessage != "which branch?" {
		t.Errorf("the client saw message %q, want %q", sawMessage, "which branch?")
	}

	// The scope is the operation's, and this is what it chose: the session the
	// handle names, and the tool call the params named within it.
	scope, ok := sawScope.(*acp.ElicitationFormModeSession)
	if !ok {
		t.Fatalf("the scope arrived as %T, want a session scope", sawScope)
	}
	if scope.SessionID != "sess-1" {
		t.Errorf("the scope names session %q, want sess-1", scope.SessionID)
	}
	if id, present := scope.ToolCallID.Get(); !present || id != "call-1" {
		t.Errorf("the scope names tool call %q (present %t), want call-1", id, present)
	}

	accepted, ok := answer.Value.(*acp.ElicitationAcceptAction)
	if !ok {
		t.Fatalf("the answer is %T, want an accept action", answer.Value)
	}
	content, present := accepted.Content.Get()
	if !present {
		t.Fatal("the accept action carries no content")
	}
	value, ok := content["branch"].(*acp.ElicitationContentValueString)
	if !ok || string(*value) != "main" {
		t.Errorf("the user answered %#v, want the string main", content["branch"])
	}
}

// A URL elicitation is the one exchange that outlives its own response: the
// client says it has shown the page, and the agent says later that the page is
// finished.
func TestAURLElicitationIsFinishedByItsCompletion(t *testing.T) {
	showing := make(chan acp.ElicitationID, 1)
	finished := make(chan acp.ElicitationID, 1)

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				_ context.Context,
				_ *acp.CreateElicitationRequest,
				mode *acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				showing <- mode.ElicitationID
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{},
				}, nil
			},
			Complete: func(_ context.Context, notification *acp.CompleteElicitationNotification) {
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
	if shown := <-showing; shown != "elicit-1" {
		t.Errorf("the client was shown %q, want elicit-1", shown)
	}
	if done := <-finished; done != "elicit-1" {
		t.Errorf("the client was told %q finished, want elicit-1", done)
	}
}

// The request scope names the call the agent is answering, and the agent never
// spells the identifier: it comes from the context the handler was given.
//
// This is the case the schema describes — an agent eliciting during
// authentication, before any session exists — so it is driven from an
// authenticate handler, which has no session and reaches the client through the
// connection it is given.
func TestARequestScopedElicitationNamesTheRequestBeingServed(t *testing.T) {
	scopes := make(chan acp.ElicitationFormModeValue, 1)

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context,
				_ *acp.CreateElicitationRequest,
				mode *acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				scopes <- mode.Value
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var elicitErr error
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(
			context.Context,
			*acp.AgentConn,
			*acp.NewSessionRequest,
		) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(
			context.Context,
			*acp.AgentSession,
			*acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel:      func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodAgent{ID: "token", Name: "Token"}},

		// No session exists here, and none will until the client is authenticated.
		// The connection is the handle, and the request being answered is the scope.
		Authenticate: func(
			ctx context.Context,
			conn *acp.AgentConn,
			_ *acp.AuthenticateRequest,
		) (*acp.AuthenticateResponse, error) {
			_, elicitErr = conn.CreateElicitation(ctx, &acp.CreateElicitationParams{
				Message: "paste your token",
				Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
			})
			return &acp.AuthenticateResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	conn := connectPeers(t, client, agent)
	if _, err := conn.Authenticate(context.Background(), &acp.AuthenticateRequest{
		MethodID: "token",
	}); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if elicitErr != nil {
		t.Fatalf("CreateElicitation: %v", elicitErr)
	}

	// A request scope rather than a session scope, and the client can tell which
	// without being handed the identifier that distinguishes them.
	if scope := <-scopes; !isRequestScope(scope) {
		t.Fatalf("the scope arrived as %T, want a request scope", scope)
	}
}

func isRequestScope(scope acp.ElicitationFormModeValue) bool {
	_, request := scope.(*acp.ElicitationFormModeRequest)
	return request
}

// The outbound half of the mode capability. A client that renders forms and not
// pages is not asked for a page, and the refusal happens before the write.
func TestAModeTheClientDidNotAdvertiseIsRefusedBeforeTheWire(t *testing.T) {
	var served int
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				served++
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var urlErr, completeErr error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, urlErr = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "sign in",
			Mode:    &acp.ElicitationURLMode{ElicitationID: "e-1", URL: "https://example.invalid/"},
		})
		completeErr = session.Conn().CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
			ElicitationID: "e-1",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The mode is a parameter capability, so the refusal is invalid-params: the
	// method is there and it is what was put inside it that was not advertised.
	var refusal *acp.Error
	if !errors.As(urlErr, &refusal) || refusal.Code != acp.ErrorCodeInvalidParams {
		t.Fatalf("a url elicitation to a form-only client failed with %v, want invalid params", urlErr)
	}
	if !strings.Contains(refusal.Message, "clientCapabilities.elicitation.url") {
		t.Errorf("the refusal says %q, which does not name the capability", refusal.Message)
	}

	// The completion is gated on the url mode as a method, so it is method-not-found.
	var completion *acp.Error
	if !errors.As(completeErr, &completion) || completion.Code != acp.ErrorCodeMethodNotFound {
		t.Fatalf("elicitation/complete to a form-only client failed with %v, want method not found", completeErr)
	}
	if served != 0 {
		t.Error("a refused elicitation still reached the client's handler")
	}
}

// The scope belongs to the operation, so a caller who sets one is told rather
// than having it replaced in silence.
func TestASuppliedScopeIsRefused(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var scoped, unknownMode, missingMode error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, scoped = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode: &acp.ElicitationFormMode{
				RequestedSchema: formSchema(),
				Value:           &acp.ElicitationFormModeSession{},
			},
		})
		_, unknownMode = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode:    &acp.CreateElicitationRequestOther{},
		})
		_, missingMode = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if scoped == nil || !strings.Contains(scoped.Error(), "scope") {
		t.Errorf("a mode carrying its own scope was answered %v, want a refusal naming the scope", scoped)
	}
	if unknownMode == nil || !strings.Contains(unknownMode.Error(), "ElicitationFormMode") {
		t.Errorf("the catch-all mode was answered %v, want a refusal naming the two sendable modes", unknownMode)
	}
	if missingMode == nil {
		t.Error("params with no mode were accepted")
	}
}

// The two rules that make an advertisement servable, and the one that keeps a
// tool call out of a scope that has no session.
func TestElicitationConfigurationIsRefusedWhenItCannotBeServed(t *testing.T) {
	form := func(
		context.Context,
		*acp.CreateElicitationRequest,
		*acp.ElicitationFormMode,
	) (*acp.CreateElicitationResponse, error) {
		return &acp.CreateElicitationResponse{}, nil
	}
	url := func(
		context.Context,
		*acp.CreateElicitationRequest,
		*acp.ElicitationURLMode,
	) (*acp.CreateElicitationResponse, error) {
		return &acp.CreateElicitationResponse{}, nil
	}
	complete := func(context.Context, *acp.CompleteElicitationNotification) {}

	for name, handlers := range map[string]*acp.ElicitationHandlers{
		"neither mode":                  {},
		"a url mode with no completion": {URL: url},
		"a completion with no url mode": {Form: form, Complete: complete},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := acp.NewClient(&acp.ClientConfig{
				SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
				RequestPermission: denyingPermission,
				Elicitation:       handlers,
			})
			if err == nil {
				t.Fatal("the handler group was accepted and could not serve what it advertises")
			}
		})
	}
}

// A request-scoped elicitation has no session, so the tool call a session scope
// would carry is refused rather than dropped.
func TestARequestScopeRefusesAToolCall(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var withToolCall error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, withToolCall = session.Conn().CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message:    "paste your token",
			Mode:       &acp.ElicitationFormMode{RequestedSchema: formSchema()},
			ToolCallID: acp.OptValue(acp.ToolCallID("call-1")),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if withToolCall == nil || !strings.Contains(withToolCall.Error(), "ToolCallID") {
		t.Errorf("a tool call on a request scope was answered %v, want a refusal naming it", withToolCall)
	}
}

// Called outside a handler there is no request to be scoped to, and inventing one
// would send the client an elicitation tied to a call that is not happening.
func TestARequestScopeOutsideAHandlerIsRefused(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	agentConn := make(chan *acp.AgentConn, 1)
	agent := testAgent(t, func(
		_ context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		agentConn <- session.Conn()
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}

	// The prompt has been answered, so this context belongs to no request.
	conn := <-agentConn
	_, err = conn.CreateElicitation(context.Background(), &acp.CreateElicitationParams{
		Message: "paste your token",
		Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
	})
	if err == nil || !strings.Contains(err.Error(), "must be called from a handler") {
		t.Errorf("eliciting outside a handler was answered %v, want a refusal saying where it belongs", err)
	}
}

func stringContent(value string) *acp.ElicitationContentValueString {
	content := acp.ElicitationContentValueString(value)
	return &content
}
