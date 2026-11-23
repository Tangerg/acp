package acp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestAnAgentRejectsAnUnsolicitedPermissionSelectionFromTheWire(t *testing.T) {
	stream, conn := rawAgentConnection(t)
	t.Cleanup(func() { _ = conn.Close() })
	handshakeRaw(t.Context(), t, stream)
	session := conn.session("session")
	for _, selected := range []PermissionOptionID{"unoffered", "allow"} {
		result := make(chan error, 1)
		go func() {
			_, err := session.RequestPermission(t.Context(), &RequestPermissionParams{
				ToolCall: ToolCallUpdate{ToolCallID: "operation"},
				Options:  []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: PermissionOptionKindAllowOnce}},
			})
			result <- err
		}()
		message, err := stream.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		call, ok := message.(*jsonrpc.Request)
		if !ok || call.Method != methodSessionRequestPermission {
			t.Fatalf("read %T, want a permission request", message)
		}
		answer, err := json.Marshal(&RequestPermissionResponse{Outcome: &SelectedPermissionOutcome{OptionID: selected}})
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Write(t.Context(), &jsonrpc.Response{ID: call.ID, Result: answer}); err != nil {
			t.Fatal(err)
		}
		err = <-result
		if (err == nil) != (selected == "allow") {
			t.Errorf("selected %q returned %v", selected, err)
		}
	}
}

func TestDuplicatePermissionIdentitiesFromTheWireNeverReachTheUI(t *testing.T) {
	var delivered atomic.Bool
	client, err := NewClient(&ClientConfig{
		SessionUpdate: func(context.Context, *ClientConn, *SessionNotification) {},
		RequestPermission: func(context.Context, *ClientConn, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
			delivered.Store(true)
			return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := connectRawClient(t, client)
	request := rawCall(2, methodSessionRequestPermission, `{"sessionId":"same-session","toolCall":{"toolCallId":"tool"},"options":[{"optionId":"same","name":"Allow","kind":"allow_once"},{"optionId":"same","name":"Deny","kind":"reject_once"}]}`)
	if err := stream.Write(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
		t.Errorf("duplicate options returned %s, want invalid params", code)
	}
	if delivered.Load() {
		t.Error("ambiguous option identities reached the permission UI")
	}
}
