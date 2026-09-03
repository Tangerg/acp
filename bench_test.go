package acp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp"
)

// A regression guard, not an optimisation target: nothing here has been tuned, and
// a number one of these produces is not a reason to tune anything. What they catch
// is a schema bump or codec change that makes the per-message path several times
// more expensive, which every other check in this repository misses because
// correctness is unaffected.
//
// The message is the one a turn sends most — an agent streaming its answer emits
// one session/update per chunk — so this is where a long turn spends its time.

func benchmarkNotification() *acp.SessionNotification {
	return &acp.SessionNotification{
		SessionID: "sess-1",
		Update: &acp.AgentMessageChunk{
			ContentChunk: acp.ContentChunk{Content: &acp.TextContent{Text: "the quick brown fox jumps over the lazy dog"}},
		},
	}
}

func BenchmarkEncodeSessionNotification(b *testing.B) {
	notification := benchmarkNotification()
	b.ReportAllocs()

	for b.Loop() {
		if _, err := json.Marshal(notification); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeSessionNotification(b *testing.B) {
	encoded, err := json.Marshal(benchmarkNotification())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()

	for b.Loop() {
		var decoded acp.SessionNotification
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			b.Fatal(err)
		}
	}
}

// The one that would show a regression in the ordered delivery stage rather than
// in the codec: the queue is what keeps a turn's updates ahead of its answer.
func BenchmarkPromptTurn(b *testing.B) {
	const updatesPerTurn = 16

	ctx := b.Context()

	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			for range updatesPerTurn {
				err := session.Update(ctx, &acp.SessionUpdateParams{
					Update: &acp.AgentMessageChunk{
						ContentChunk: acp.ContentChunk{Content: &acp.TextContent{Text: "the quick brown fox jumps over the lazy dog"}},
					},
				})
				if err != nil {
					return nil, err
				}
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		b.Fatal(err)
	}

	var received int
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) { received++ },
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return &acp.RequestPermissionResponse{}, nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		b.Fatal(err)
	}

	prompt := &acp.PromptParams{Prompt: []acp.ContentBlock{&acp.TextContent{Text: "go"}}}
	b.ReportAllocs()

	for b.Loop() {
		if _, err := session.Prompt(ctx, prompt); err != nil {
			b.Fatal(err)
		}
	}

	// Reported rather than asserted: a benchmark that fails is a test in the wrong
	// file. A turn that delivered no updates would still be worth noticing in the
	// output, because it would mean this measured an empty path.
	b.ReportMetric(float64(received)/float64(b.N), "updates/turn")
}
