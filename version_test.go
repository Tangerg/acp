package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// The constant is what goes on the wire in initialize, so the test states the
// number rather than importing it back out of the package under test: a test that
// reads its expectation from the code it checks passes however that code changes.
func TestCurrentProtocolVersionIsTheImplementedProtocolMajor(t *testing.T) {
	if acp.CurrentProtocolVersion != 1 {
		t.Fatalf("CurrentProtocolVersion = %d, want 1", acp.CurrentProtocolVersion)
	}
}

// initialize carries the version as a JSON number, not a string or a "v1" label.
// Encoding it here is the caller-side form of that promise.
func TestCurrentProtocolVersionEncodesAsJSONNumber(t *testing.T) {
	encoded, err := json.Marshal(struct {
		ProtocolVersion acp.ProtocolVersion `json:"protocolVersion"`
	}{acp.CurrentProtocolVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"protocolVersion":1}`; got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
}

// What the two sides do with a protocol version that is not this package's.
//
// The rule is the schema's, quoted from InitializeResponse.protocolVersion: the
// agent answers with "the protocol version the client specified if supported by
// the agent, or the latest protocol version supported by the agent", and "the
// client should disconnect, if it doesn't support this version".
//
// This package implements version 1 and no other, so the agent's answer is always
// 1 and the client proceeds only on 1. Both halves are worth a test because both
// were wrong: the agent answered the lower of the two, which told a client asking
// for 0 that it had got 0 and left this side claiming a grammar it does not have.

// The agent answers with the version it implements, whatever it was asked for.
func TestTheAgentAnswersWithTheVersionItImplements(t *testing.T) {
	for _, version := range []acp.ProtocolVersion{
		0,                              // older than anything this package speaks
		acp.CurrentProtocolVersion,     // the one it does
		acp.CurrentProtocolVersion + 1, // a client from the future
		9999,
	} {
		t.Run(fmt.Sprintf("asked for %d", version), func(t *testing.T) {
			conn, stream, ctx := rawClientFor(t, testAgent(t, nil))

			params := fmt.Sprintf(`{"protocolVersion":%d,"clientCapabilities":{}}`, version)
			response := roundTrip(ctx, t, stream, 1, "initialize", params)
			if response.Error != nil {
				t.Fatalf("the agent refused initialize: %v", response.Error)
			}

			var answered acp.InitializeResponse
			if err := json.Unmarshal(response.Result, &answered); err != nil {
				t.Fatalf("decoding the answer: %v", err)
			}
			if answered.ProtocolVersion != acp.CurrentProtocolVersion {
				t.Fatalf("the agent answered version %d for a request of %d; the schema says the "+
					"version the client asked for if the agent supports it and the agent's latest "+
					"otherwise, and this package supports %d and no other",
					answered.ProtocolVersion, version, acp.CurrentProtocolVersion)
			}

			// And the negotiated version is what the connection reports, rather
			// than what the peer asked for.
			if got := conn.Peer().ProtocolVersion; got != acp.CurrentProtocolVersion {
				t.Errorf("the connection reports version %d", got)
			}
		})
	}
}

// A second initialize is not a re-negotiation. Capabilities gate work already in
// flight, and letting them change under it would make an authority boundary depend
// on timing.
func TestASecondInitializeIsRefused(t *testing.T) {
	_, stream, ctx := rawClientFor(t, testAgent(t, nil))

	const params = `{"protocolVersion":1,"clientCapabilities":{}}`
	if response := roundTrip(ctx, t, stream, 1, "initialize", params); response.Error != nil {
		t.Fatalf("the first initialize was refused: %v", response.Error)
	}
	if response := roundTrip(ctx, t, stream, 2, "initialize", params); response.Error == nil {
		t.Fatal("a second initialize was accepted, so the capabilities gating work in flight " +
			"could change under it")
	}
}

// The client disconnects rather than speak a grammar it does not have.
//
// A protocol number identifies a grammar, not a feature level whose minimum is
// automatically safe: an answer of 0 is as unusable as an answer of 99. Both are
// refused, and Connect hands back nothing to use.
func TestTheClientRefusesAVersionItDoesNotSpeak(t *testing.T) {
	tests := map[string]struct {
		answered acp.ProtocolVersion
		accepted bool
	}{
		"older than this package speaks":  {answered: 0},
		"the version this package speaks": {answered: acp.CurrentProtocolVersion, accepted: true},
		"newer than this package speaks":  {answered: acp.CurrentProtocolVersion + 1},
		"far newer":                       {answered: 9999},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			transport := &answersVersion{version: test.answered}
			conn, err := testClient(t).Connect(context.Background(), transport)

			if test.accepted {
				if err != nil {
					t.Fatalf("Connect: %v", err)
				}
				defer conn.Close() //nolint:errcheck // idempotent.
				if got := conn.Peer().ProtocolVersion; got != acp.CurrentProtocolVersion {
					t.Fatalf("the connection reports version %d", got)
				}
				return
			}

			if err == nil {
				_ = conn.Close()
				t.Fatalf("Connect accepted an agent speaking version %d", test.answered)
			}
			if conn != nil {
				t.Error("Connect returned both an error and a connection")
			}
			if !strings.Contains(err.Error(), "protocol version") {
				t.Errorf("Connect failed with %v, which does not say the version was the problem", err)
			}
			// Disconnected, not merely refused: nothing is left reading a stream
			// whose grammar nobody agreed.
			if err := transport.wait(); err != nil {
				t.Errorf("the transport was left open: %v", err)
			}
		})
	}
}

// answersVersion answers initialize with a version of the test's choosing, and
// nothing else at all.
type answersVersion struct {
	version acp.ProtocolVersion

	mu      sync.Mutex
	replies []jsonrpc.Message
	closed  chan struct{}
	once    sync.Once
}

func (t *answersVersion) Connect(context.Context) (acp.Connection, error) {
	t.closed = make(chan struct{})
	return t, nil
}

func (t *answersVersion) Write(_ context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok || !request.IsCall() {
		return nil
	}
	result := fmt.Sprintf(`{"protocolVersion":%d,"agentCapabilities":{}}`, t.version)
	encoded, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: request.ID, Result: []byte(result)})
	if err != nil {
		return err
	}
	decoded, err := jsonrpc.DecodeMessage(encoded)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.replies = append(t.replies, decoded)
	t.mu.Unlock()
	return nil
}

func (t *answersVersion) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		t.mu.Lock()
		if len(t.replies) > 0 {
			reply := t.replies[0]
			t.replies = t.replies[1:]
			t.mu.Unlock()
			return reply, nil
		}
		t.mu.Unlock()

		select {
		case <-t.closed:
			return nil, io.EOF
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (t *answersVersion) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// wait reports whether the transport was closed, within a bound, so that a failure
// is a failure rather than a hung test.
func (t *answersVersion) wait() error {
	select {
	case <-t.closed:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("still open")
	}
}
