package acp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// The copy boundary, held against the values it exists to protect.
//
// A shallow copy of the outermost struct was not enough and never could be. The
// capability tree nests a reserved _meta map inside every group, an auth method
// carries argument slices and environment maps, and _meta is arbitrary JSON that
// may be maps inside maps. Replacing an outer slice proves nothing about any of
// them.

// Mutating a caller's configuration after construction changes nothing the agent
// advertises.
func TestAnAgentDoesNotShareItsConfigurationWithItsCaller(t *testing.T) {
	terminal := &acp.AuthMethodTerminal{
		ID:   "tui",
		Name: "Sign in with a terminal",
		Args: []string{"--auth"},
		Env:  acp.OptValue(map[string]string{"MODE": "auth"}),
		Meta: acp.OptValue(acp.Meta{"nested": map[string]any{"depth": 2}}),
	}
	config := &acp.AgentConfig{
		Info: &acp.Implementation{
			Name:    "agent",
			Version: "1",
			Meta:    acp.OptValue(acp.Meta{"nested": map[string]any{"depth": 2}}),
		},
		Meta:        acp.Meta{"nested": map[string]any{"depth": 2}},
		AuthMethods: []acp.AuthMethod{terminal},
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		Capabilities: &acp.AgentCapabilities{
			SessionCapabilities: acp.SessionCapabilities{
				Meta: acp.OptValue(acp.Meta{"nested": map[string]any{"depth": 2}}),
			},
			Meta: acp.OptValue(acp.Meta{"nested": map[string]any{"depth": 2}}),
		},
	}

	agent, err := acp.NewAgent(config)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// Every mutation is of something nested, because replacing the outer value is
	// the case a shallow copy already survives.
	mutateEverything(config)
	terminal.Args[0] = "--tampered"
	terminal.ID = "tampered"

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Capabilities:      &acp.ClientCapabilities{Auth: acp.AuthCapabilities{Terminal: true}},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session := connectAndOpen(t, client, agent)
	peer := session.Conn().Peer()

	if encoded := text(t, peer.AgentMeta); strings.Contains(encoded, "tampered") {
		t.Errorf("the agent's _meta went out as %s", encoded)
	}
	if encoded := text(t, peer.AgentCapabilities); strings.Contains(encoded, "tampered") {
		t.Errorf("the agent's capabilities went out as %s", encoded)
	}
	if encoded := text(t, peer.AgentInfo); strings.Contains(encoded, "tampered") {
		t.Errorf("the agent's info went out as %s", encoded)
	}
	if encoded := text(t, peer.AuthMethods); strings.Contains(encoded, "tampered") {
		t.Errorf("the agent's authentication methods went out as %s", encoded)
	}
}

// The snapshot Peer returns is one the caller may do anything to.
func TestPeerHandsOutASnapshotNobodyElseHolds(t *testing.T) {
	agent, err := acp.NewAgent(&acp.AgentConfig{
		Info: &acp.Implementation{
			Name:    "agent",
			Version: "1",
			Meta:    acp.OptValue(acp.Meta{"nested": map[string]any{"depth": 2}}),
		},
		Meta: acp.Meta{"nested": map[string]any{"depth": 2}},
		AuthMethods: []acp.AuthMethod{
			&acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"},
		},
		Authenticate: func(context.Context, *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			return &acp.AuthenticateResponse{}, nil
		},
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	session := connectAndOpen(t, testClient(t), agent)
	conn := session.Conn()

	// Reach as deep as the value goes and change it.
	held := conn.Peer()
	if meta, ok := held.AgentMeta.Get(); ok {
		if nested, ok := meta["nested"].(map[string]any); ok {
			nested["depth"] = "tampered"
		}
		meta["added"] = "tampered"
	}
	if method, ok := held.AuthMethods[0].(*acp.AuthMethodAgent); ok {
		method.ID = "tampered"
	}
	held.AgentCapabilities.LoadSession = true

	fresh := conn.Peer()
	if encoded := text(t, fresh); strings.Contains(encoded, "tampered") {
		t.Fatalf("a second Peer reported %s, so the first snapshot was the connection's own", encoded)
	}
	if fresh.AgentCapabilities.LoadSession {
		t.Error("the capability gate reads a value a caller can widen")
	}
}

// A _meta value keeps the types the caller put in it. A copy that round-tripped
// through JSON would hand back every number as a float64, which is a different
// value from the one that went in.
func TestCopyingMetaKeepsTheTypesItWasGiven(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Meta:              acp.Meta{"count": 3, "nested": map[string]any{"count": 4}},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session := connectAndOpen(t, client, testAgent(t, nil))

	meta, ok := session.Conn().Peer().ClientMeta.Get()
	if !ok {
		t.Fatal("the snapshot carries no client _meta")
	}
	count, counted := meta["count"].(int)
	if !counted || count != 3 {
		t.Fatalf("count came back as %#v, want the int that went in", meta["count"])
	}
	nested, nestedOK := meta["nested"].(map[string]any)
	if !nestedOK {
		t.Fatalf("nested came back as %#v", meta["nested"])
	}
	inner, innerOK := nested["count"].(int)
	if !innerOK || inner != 4 {
		t.Fatalf("nested count came back as %#v", nested["count"])
	}
}

// mutateEverything reaches into every nested value a configuration holds and
// changes it, which is what a shallow copy fails to survive.
func mutateEverything(config *acp.AgentConfig) {
	tamper := func(meta acp.Meta) {
		if meta == nil {
			return
		}
		if nested, ok := meta["nested"].(map[string]any); ok {
			nested["depth"] = "tampered"
		}
		meta["added"] = "tampered"
	}
	tamperOpt := func(meta acp.Opt[acp.Meta]) {
		if value, ok := meta.Get(); ok {
			tamper(value)
		}
	}

	tamper(config.Meta)
	tamperOpt(config.Info.Meta)
	config.Info.Name = "tampered"
	tamperOpt(config.Capabilities.Meta)
	tamperOpt(config.Capabilities.SessionCapabilities.Meta)
	if method, ok := config.AuthMethods[0].(*acp.AuthMethodTerminal); ok {
		tamperOpt(method.Meta)
		if env, ok := method.Env.Get(); ok {
			env["MODE"] = "tampered"
		}
	}
}

// text encodes a value the way it would go on the wire, so that a test can ask
// one question of a whole tree.
func text(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", reflect.TypeOf(value), err)
	}
	return string(encoded)
}
