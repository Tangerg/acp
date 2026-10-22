package acp_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
)

func TestCallRetainsTheResultUntilDecodingFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent, err := acp.NewAgent(&acp.AgentConfig{
			NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
				return &acp.NewSessionResponse{SessionID: "session"}, nil
			},
			Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
				return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			},
			Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
			CallFallback: func(context.Context, *acp.ExtRequest) (json.RawMessage, error) {
				return json.RawMessage(`{"value":42}`), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		conn := connectPeers(t, testClient(t), agent)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := &pausedResult{entered: make(chan struct{}), release: make(chan struct{})}
		returned := make(chan error, 1)
		go func() { returned <- conn.Call(ctx, "_test/result", nil, result) }()
		<-result.entered
		cancel()
		synctest.Wait()
		early := false
		select {
		case callErr := <-returned:
			early = true
			t.Errorf("Call returned %v while its decoder still owned the caller's result", callErr)
		default:
		}
		close(result.release)
		if !early {
			if err := <-returned; err != nil {
				t.Fatalf("completed decode: %v", err)
			}
		}
		synctest.Wait()
		if result.value != 42 {
			t.Fatalf("decoded value = %d, want 42", result.value)
		}
	})
}

type pausedResult struct {
	entered chan struct{}
	release chan struct{}
	value   int
}

func (p *pausedResult) UnmarshalJSON(data []byte) error {
	close(p.entered)
	<-p.release
	var decoded struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	p.value = decoded.Value
	return nil
}

func TestAResultDecoderCanCallTheSameConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent, err := acp.NewAgent(&acp.AgentConfig{
			NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
				return &acp.NewSessionResponse{SessionID: "session"}, nil
			},
			Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
				return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			},
			Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
			CallFallback: func(context.Context, *acp.ExtRequest) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		conn := connectPeers(t, testClient(t), agent)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result := &reentrantResult{decode: func() error {
			var nested json.RawMessage
			return conn.Call(ctx, "_test/nested", nil, &nested)
		}}
		if err := conn.Call(ctx, "_test/outer", nil, result); err != nil {
			t.Fatalf("decoder could not use its connection: %v", err)
		}
	})
}

type reentrantResult struct{ decode func() error }

func (r *reentrantResult) UnmarshalJSON([]byte) error { return r.decode() }
