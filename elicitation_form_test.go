package acp_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/acp"
)

func TestAnInvalidFormSchemaNeverReachesThePeer(t *testing.T) {
	var rendered atomic.Int32
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(context.Context, *acp.ClientConn, *acp.CreateElicitationRequest, *acp.ElicitationFormMode) (*acp.CreateElicitationResponse, error) {
				rendered.Add(1)
				return &acp.CreateElicitationResponse{Value: &acp.CreateElicitationResponseDecline{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var elicitationErr error
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, elicitationErr = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "invalid form",
			Mode: &acp.ElicitationFormMode{RequestedSchema: acp.ElicitationSchema{
				Type: acp.ElicitationSchemaTypeObject,
				Properties: map[string]acp.ElicitationPropertySchema{
					"text": &acp.StringPropertySchema{Pattern: acp.OptValue("[")},
				},
			}},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})
	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(t.Context(), &acp.PromptParams{}); err != nil {
		t.Fatal(err)
	}
	if elicitationErr == nil || !strings.Contains(elicitationErr.Error(), "invalid elicitation schema") {
		t.Errorf("invalid local schema returned %v", elicitationErr)
	}
	if rendered.Load() != 0 {
		t.Fatal("the peer was asked to render an invalid local form")
	}
}

// A form elicitation crosses the connection, reaches the mode's handler, and the
// user's answer comes back to the agent.
func TestAFormElicitationReachesTheUserAndAnswers(t *testing.T) {
	var sawMessage string
	var sawScope acp.ElicitationFormModeValue

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context, _ *acp.ClientConn, request *acp.CreateElicitationRequest,
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
	if scope.SessionID != testSessionID {
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

func stringContent(value string) *acp.ElicitationContentValueString {
	content := acp.ElicitationContentValueString(value)
	return &content
}
