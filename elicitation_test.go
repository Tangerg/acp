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

// A URL elicitation is the one exchange that can outlive its own response: an
// accept consents to start the out-of-band interaction, and the agent says later
// that the accepted interaction is finished.
func TestAURLElicitationIsFinishedByItsCompletion(t *testing.T) {
	accepted := make(chan acp.ElicitationID, 1)
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
				accepted <- mode.ElicitationID
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
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context,
				*acp.CreateElicitationRequest,
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
			Complete: func(context.Context, *acp.CompleteElicitationNotification) {},
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

// A group that could not serve what it advertises is refused. Serving the URL
// mode without a completion handler is not one of those: the protocol makes
// sending a completion optional, so a client that never hears about one is a
// client that chose not to.
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

	t.Run("a url mode with no completion", func(t *testing.T) {
		_, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
			RequestPermission: denyingPermission,
			Elicitation:       &acp.ElicitationHandlers{URL: url},
		})
		if err != nil {
			t.Fatalf("a client that shows pages and does not want to hear they finished "+
				"was refused, and the protocol makes the completion optional: %v", err)
		}
	})
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

// The contracts a caller can break without a peer being involved at all. They are
// refused before anything is sent, and the message says which field.
func TestElicitationRefusesWhatACallerGetsWrong(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
			Complete: func(context.Context, *acp.CompleteElicitationNotification) {},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var noParams, noCompletionParams, scopedURL error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, noParams = session.CreateElicitation(ctx, nil)
		noCompletionParams = session.Conn().CompleteElicitation(ctx, nil)
		_, scopedURL = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "sign in",
			Mode: &acp.ElicitationURLMode{
				ElicitationID: "e-1",
				URL:           "https://example.invalid/",
				Value:         &acp.ElicitationURLModeSession{},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}

	for name, test := range map[string]struct {
		err   error
		wants string
	}{
		"an elicitation with no params":     {noParams, "Message and Mode"},
		"a completion with no params":       {noCompletionParams, "ElicitationID"},
		"a url mode carrying its own scope": {scopedURL, "scope"},
	} {
		t.Run(name, func(t *testing.T) {
			if test.err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(test.err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q", test.err, test.wants)
			}
		})
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
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context,
				*acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				accepted <- struct{}{}
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
			Complete: func(context.Context, *acp.CompleteElicitationNotification) {},
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

// A request-scoped elicitation is the schema's "tied to a specific JSON-RPC
// request outside of a session". A prompt has a session, so an elicitation from
// one belongs to that session — scoping it to the request would name a call the
// client already knows is part of a conversation.
func TestARequestScopeRefusesASessionScopedRequest(t *testing.T) {
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

	var fromPrompt error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, fromPrompt = session.Conn().CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}
	if fromPrompt == nil || !strings.Contains(fromPrompt.Error(), "scoped to a session") {
		t.Errorf("a request scope taken from a prompt was answered %v, want a refusal naming "+
			"the session the request already has", fromPrompt)
	}
}

// The bound on outstanding URL elicitations comes from the connection's own
// Limits, and reaching it refuses the elicitation rather than ending the
// connection — the one bound in limits.go that behaves that way, because either
// peer may originate one and a refusal costs nothing already in flight.
//
// Driven through the public API on purpose. The reservation reads the bound from
// the connection that resolved it, and a test that supplied its own would pass
// with that wiring removed.
func TestTheOutstandingURLElicitationBoundIsTheConfiguredOne(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context,
				*acp.CreateElicitationRequest,
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
		Limits: acp.Limits{OutstandingElicitations: 1},
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
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
	if second == nil || !strings.Contains(second.Error(), "URL elicitation limit exceeded (limit 1)") {
		t.Fatalf("the elicitation past the bound was answered %v, want the configured bound", second)
	}

	// Still usable: this bound refuses the operation and nothing else.
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("the connection did not survive a refused elicitation: %v", err)
	}
}
