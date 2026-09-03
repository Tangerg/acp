package acp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

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
		Meta: acp.OptValue(mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}})),
	}
	config := &acp.AgentConfig{
		Info: &acp.Implementation{
			Name:    "agent",
			Version: "1",
			Meta:    acp.OptValue(mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}})),
		},
		Meta:        mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}}),
		AuthMethods: []acp.AuthMethod{terminal},
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		Capabilities: &acp.AgentCapabilities{
			SessionCapabilities: acp.SessionCapabilities{
				Meta: acp.OptValue(mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}})),
			},
			Meta: acp.OptValue(mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}})),
		},
	}

	agent, err := acp.NewAgent(config)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// Every mutation is of something nested, because replacing the outer value is
	// the case a shallow copy already survives.
	mutateEverything(t, config)
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
			Meta:    acp.OptValue(mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}})),
		},
		Meta: mustMeta(t, map[string]any{"nested": map[string]any{"depth": 2}}),
		AuthMethods: []acp.AuthMethod{
			&acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"},
		},
		Authenticate: func(context.Context, *acp.AgentConn, *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			return &acp.AuthenticateResponse{}, nil
		},
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
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
		if err := meta.Set("nested", map[string]any{"depth": "tampered"}); err != nil {
			t.Fatalf("tamper nested metadata: %v", err)
		}
		if err := meta.Set("added", "tampered"); err != nil {
			t.Fatalf("tamper metadata: %v", err)
		}
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

// Decoding into a caller-selected type keeps the JSON boundary explicit without
// forcing every number through interface{} as float64.
func TestMetaDecodesIntoTheRequestedType(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Meta:              mustMeta(t, map[string]any{"count": 3, "nested": map[string]any{"count": 4}}),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session := connectAndOpen(t, client, testAgent(t, nil))

	meta, ok := session.Conn().Peer().ClientMeta.Get()
	if !ok {
		t.Fatal("the snapshot carries no client _meta")
	}
	var count int
	if ok, err := meta.Decode("count", &count); err != nil || !ok || count != 3 {
		t.Fatalf("Decode(count) = (%d, %t, %v), want (3, true, nil)", count, ok, err)
	}
	var nested struct {
		Count int `json:"count"`
	}
	if ok, err := meta.Decode("nested", &nested); err != nil || !ok || nested.Count != 4 {
		t.Fatalf("Decode(nested) = (%+v, %t, %v), want count 4", nested, ok, err)
	}
}

// mutateEverything reaches into every nested value a configuration holds and
// changes it, which is what a shallow copy fails to survive.
func mutateEverything(t *testing.T, config *acp.AgentConfig) {
	t.Helper()
	tamper := func(meta acp.Meta) {
		if err := meta.Set("nested", map[string]any{"depth": "tampered"}); err != nil {
			t.Fatalf("tamper nested metadata: %v", err)
		}
		if err := meta.Set("added", "tampered"); err != nil {
			t.Fatalf("tamper metadata: %v", err)
		}
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

// Encoding at insertion time prevents a later mutation of a Go object from
// changing metadata that has already crossed the configuration boundary.
func TestMetaDoesNotRetainArbitraryGoObjects(t *testing.T) {
	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	value := map[string]any{"stamp": stamp}
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Meta:              mustMeta(t, map[string]any{"stamp": stamp, "nested": value}),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session := connectAndOpen(t, client, testAgent(t, nil))

	meta, ok := session.Conn().Peer().ClientMeta.Get()
	if !ok {
		t.Fatal("the snapshot carries no client _meta")
	}
	value["stamp"] = "tampered"
	var kept time.Time
	if ok, err := meta.Decode("stamp", &kept); err != nil || !ok || !kept.Equal(stamp) {
		t.Fatalf("Decode(stamp) = (%v, %t, %v), want %v", kept, ok, err, stamp)
	}
	var nested struct {
		Stamp time.Time `json:"stamp"`
	}
	if ok, err := meta.Decode("nested", &nested); err != nil || !ok || !nested.Stamp.Equal(stamp) {
		t.Fatalf("Decode(nested) = (%v, %t, %v), want %v", nested.Stamp, ok, err, stamp)
	}
}

func mustMeta(t *testing.T, values map[string]any) acp.Meta {
	t.Helper()
	meta, err := acp.NewMeta(values)
	if err != nil {
		t.Fatalf("NewMeta: %v", err)
	}
	return meta
}

func TestMetaRejectsValuesThatCannotCrossJSON(t *testing.T) {
	if _, err := acp.NewMeta(map[string]any{"work": make(chan struct{})}); err == nil {
		t.Fatal("NewMeta accepted a channel that could only fail later while writing protocol data")
	}

	var meta acp.Meta
	if err := json.Unmarshal([]byte("null"), &meta); err == nil {
		t.Fatal("null decoded as a Meta object, collapsing null and an empty object")
	}
}
