package acp_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/acp"
)

func TestPermissionResponseMustSelectAnOfferedOption(t *testing.T) {
	for _, rewriteOptions := range []bool{false, true} {
		name := "unknown selection"
		if rewriteOptions {
			name = "handler rewrites options"
		}
		t.Run(name, func(t *testing.T) {
			client, err := acp.NewClient(&acp.ClientConfig{
				SessionUpdate: func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
				RequestPermission: func(_ context.Context, _ *acp.ClientConn, request *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
					if rewriteOptions {
						request.Options[0].OptionID = "unoffered"
					}
					return &acp.RequestPermissionResponse{Outcome: &acp.SelectedPermissionOutcome{OptionID: "unoffered"}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
				_, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
					ToolCall: acp.ToolCallUpdate{ToolCallID: "operation"},
					Options:  []acp.PermissionOption{{OptionID: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce}},
				})
				result <- err
				return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			})
			session := connectAndOpen(t, client, agent)
			if _, err := session.Prompt(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			var failure *acp.Error
			if err := <-result; !errors.As(err, &failure) || failure.Code != acp.ErrorCodeInternalError {
				t.Errorf("permission response = %v, want invalid handler response", err)
			}
		})
	}
}

func TestPermissionOptionsRejectDuplicateIdentitiesBeforeDelivery(t *testing.T) {
	var delivered atomic.Bool
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: func(context.Context, *acp.ClientConn, *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
			delivered.Store(true)
			return &acp.RequestPermissionResponse{Outcome: &acp.SelectedPermissionOutcome{OptionID: "same"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
			ToolCall: acp.ToolCallUpdate{ToolCallID: "operation"},
			Options: []acp.PermissionOption{
				{OptionID: "same", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionID: "same", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		result <- err
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})
	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "offered more than once") {
		t.Errorf("duplicate permission options = %v, want a local refusal naming the duplicate", err)
	}
	if delivered.Load() {
		t.Error("the ambiguous request reached the permission UI")
	}
}
