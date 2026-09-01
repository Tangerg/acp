package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// The interoperability evidence: transcripts recorded against an agent built on
// the reference implementation, replayed here.
//
// Two Go endpoints talking to each other share any wire bug they have, so
// everything else in this package's tests could pass while the wire was wrong.
// These are the other implementation's bytes. scripts/interop.sh produced them by
// running a real subprocess; this replays them with no network and no Node, so
// the check runs on every push rather than when somebody remembers — the same
// arrangement as the fixture corpus, for the same reason.
//
// Replaying rather than re-running does give up one thing, and it is worth being
// precise about what: it cannot catch the reference implementation changing.
// Re-recording is what catches that, which is why the updater is pinned and meant
// to be run on a schedule.

type transcript struct {
	Scenario   string `json:"scenario"`
	Provenance struct {
		Commit  string `json:"sdkCommit"`
		Version string `json:"sdkVersion"`
		Schema  string `json:"schemaRelease"`
	} `json:"provenance"`
	Observed struct {
		Updates            []string `json:"updates"`
		PermissionRequests int      `json:"permissionRequests"`
		StopReason         string   `json:"stopReason"`
		FilesRead          []string `json:"filesRead"`
		FilesWritten       []string `json:"filesWritten"`
		TerminalsCreated   int      `json:"terminalsCreated"`
		AuthRequired       bool     `json:"authRequired"`
		Authenticated      bool     `json:"authenticated"`
	} `json:"observed"`
	Messages []struct {
		From    string          `json:"from"`
		Message json.RawMessage `json:"message"`
	} `json:"messages"`
}

func TestInteroperabilityWithTheReferenceImplementation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "interop", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no transcripts found; this is the only evidence that the two implementations agree")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			//nolint:gosec // the path is a glob result under testdata.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var recorded transcript
			if err := json.Unmarshal(raw, &recorded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if recorded.Provenance.Commit == "" || recorded.Provenance.Schema == "" {
				t.Fatal("the transcript records no provenance, so it is an anecdote rather than evidence")
			}
			replay(t, &recorded)
		})
	}
}

// replay drives this package's client through a recorded scenario.
//
// The agent side is scripted: whatever the transcript says the reference
// implementation sent, this sends, in the order it sent it. What is checked is the
// other half — that this client sends the same requests, in the same order, and
// that its handlers make the same thing of the answers.
func replay(t *testing.T, recorded *transcript) {
	t.Helper()

	seen := &interopObservations{}
	client, err := interopClient(seen)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	script := &scriptedTransport{t: t, recorded: recorded, ready: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := client.Connect(ctx, script)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	switch {
	case recorded.Observed.AuthRequired:
		replayAuth(ctx, t, conn, script, seen)
	case recorded.Observed.StopReason == string(acp.StopReasonCancelled):
		replayCancelled(ctx, t, conn, script)
	default:
		replayTurn(ctx, t, conn, script, recorded)
	}

	if err := script.err(); err != nil {
		t.Fatalf("the client departed from the recorded exchange: %v", err)
	}
	if got, want := seen.snapshot(), recorded.Observed; !sameObservations(got, want) {
		t.Fatalf("the client made\n  %+v\nof the exchange, and the recording says\n  %+v", got, want)
	}
}

func replayTurn(
	ctx context.Context,
	t *testing.T,
	conn *acp.ClientConn,
	script *scriptedTransport,
	recorded *transcript,
) {
	t.Helper()
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		t.Fatalf("NewSession: %v", script.explain(err))
	}
	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: promptTextOf(recorded)}},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", script.explain(err))
	}
	if string(response.StopReason) != recorded.Observed.StopReason {
		t.Errorf("stop reason = %q, want %q", response.StopReason, recorded.Observed.StopReason)
	}
}

func replayCancelled(ctx context.Context, t *testing.T, conn *acp.ClientConn, script *scriptedTransport) {
	t.Helper()
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		t.Fatalf("NewSession: %v", script.explain(err))
	}

	prompted := make(chan *acp.PromptResponse, 1)
	go func() {
		response, err := session.Prompt(ctx, &acp.PromptParams{
			Prompt: []acp.ContentBlock{&acp.TextContent{Text: "cancel"}},
		})
		if err != nil {
			t.Errorf("Prompt: %v", script.explain(err))
			prompted <- nil
			return
		}
		prompted <- response
	}()

	// The script stops feeding the agent's side when it reaches the recorded
	// session/cancel, because that is this side's move. Waiting for it is how this
	// replays the ordering the recording captured rather than a sleep.
	script.waitForCue()
	if err := session.Cancel(ctx, nil); err != nil {
		t.Fatalf("Cancel: %v", script.explain(err))
	}

	response := <-prompted
	if response == nil {
		return
	}
	if response.StopReason != acp.StopReasonCancelled {
		t.Errorf("stop reason = %q, want cancelled", response.StopReason)
	}
}

func replayAuth(
	ctx context.Context,
	t *testing.T,
	conn *acp.ClientConn,
	script *scriptedTransport,
	seen *interopObservations,
) {
	t.Helper()
	_, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/auth"})
	if !errors.Is(err, acp.ErrAuthRequired) {
		t.Fatalf("the recorded -32000 became %v, want a match for ErrAuthRequired", script.explain(err))
	}
	seen.mu.Lock()
	seen.AuthRequired = true
	seen.mu.Unlock()

	if _, err := conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "interop"}); err != nil {
		t.Fatalf("Authenticate: %v", script.explain(err))
	}
	if _, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/auth"}); err != nil {
		t.Fatalf("NewSession after authenticating: %v", script.explain(err))
	}
	seen.mu.Lock()
	seen.Authenticated = true
	seen.mu.Unlock()
}

// promptTextOf recovers the scenario name the recording used, which is what the
// reference agent switches on.
func promptTextOf(recorded *transcript) string {
	for _, entry := range recorded.Messages {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if err := json.Unmarshal(entry.Message, &request); err != nil {
			continue
		}
		if request.Method == "session/prompt" && len(request.Params.Prompt) > 0 {
			return request.Params.Prompt[0].Text
		}
	}
	return "turn"
}

// A scriptedTransport plays the recorded agent's side.
//
// It checks this client's outbound messages against the recording as they are
// written, and feeds the recorded agent messages back in order. Request
// identifiers are mapped rather than compared: this client mints its own, and
// insisting they match the recording's would be asserting an implementation
// detail rather than the protocol.
type scriptedTransport struct {
	t        *testing.T
	recorded *transcript

	mu       sync.Mutex
	position int
	failure  error
	// ourIDs maps a recorded identifier to the one this client used, so that the
	// recorded responses can be readdressed. The values are the decoded JSON
	// rather than a jsonrpc.ID, which keeps minting identifiers out of the public
	// surface: a transport frames and unframes, and does not mint them.
	ourIDs map[string]any

	inbound chan jsonrpc.Message
	closed  chan struct{}
	once    sync.Once

	// ready fires when the script reaches a point where it is this side's move.
	ready     chan struct{}
	readyOnce sync.Once
}

func (s *scriptedTransport) Connect(context.Context) (acp.Connection, error) {
	s.inbound = make(chan jsonrpc.Message, len(s.recorded.Messages))
	s.closed = make(chan struct{})
	s.ourIDs = make(map[string]any)
	return s, nil
}

func (s *scriptedTransport) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case message := <-s.inbound:
		return message, nil
	case <-s.closed:
		return nil, acp.ErrConnectionClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Write checks one outbound message against the recording, then releases whatever
// the recording says the agent sent next.
func (s *scriptedTransport) Write(_ context.Context, message jsonrpc.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	// A departure ends the connection rather than being noted and ignored. The
	// alternative is a client left waiting for a response the script will never
	// send, which reports as a timeout — and a timeout is the least informative
	// way to say "these two implementations disagree".
	if s.position >= len(s.recorded.Messages) {
		return s.fail(fmt.Errorf("the client sent %s after the recording ended", encoded))
	}
	expected := s.recorded.Messages[s.position]
	if expected.From != "client" {
		return s.fail(fmt.Errorf("the client sent %s where the recording has the agent sending %s",
			encoded, expected.Message))
	}
	if err := s.compare(expected.Message, encoded); err != nil {
		return s.fail(err)
	}
	s.position++
	s.pump()
	return nil
}

// pump releases the recorded agent messages that follow, up to the next one this
// side is expected to send.
func (s *scriptedTransport) pump() {
	for s.position < len(s.recorded.Messages) {
		entry := s.recorded.Messages[s.position]
		if entry.From != "agent" {
			// The next move is this side's. Only one of those needs a cue — a
			// cancellation, which is not driven by having just received something
			// — and cueing on any of them would release the replay before the
			// exchange had reached the point the recording captured.
			if methodOf(entry.Message) == "session/cancel" {
				s.readyOnce.Do(func() { close(s.ready) })
			}
			return
		}
		message, err := jsonrpc.DecodeMessage(s.readdress(entry.Message))
		if err != nil {
			_ = s.fail(fmt.Errorf("the recording holds a message that does not decode: %w", err))
			return
		}
		s.position++
		s.inbound <- message
	}
	s.readyOnce.Do(func() { close(s.ready) })
}

func methodOf(message json.RawMessage) string {
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return ""
	}
	return envelope.Method
}

// compare holds one outbound message against its recording, ignoring the
// identifier and treating the rest semantically.
//
// Semantically because neither implementation promises byte identity: key order
// and number spelling may differ while the decoded value is the same. The
// identifier is remembered instead of compared, so that the recorded responses
// can be sent back under the identifier this client actually used.
func (s *scriptedTransport) compare(recorded, sent []byte) error {
	var was, is map[string]any
	if err := json.Unmarshal(recorded, &was); err != nil {
		return err
	}
	if err := json.Unmarshal(sent, &is); err != nil {
		return err
	}

	if recordedID, ok := was["id"]; ok {
		sentID, sending := is["id"]
		if !sending {
			return fmt.Errorf("the client sent %s with no identifier, and the recording has one", sent)
		}
		s.ourIDs[fmt.Sprint(recordedID)] = sentID
		delete(was, "id")
		delete(is, "id")
	}

	if !reflect.DeepEqual(was, is) {
		return fmt.Errorf("the client sent\n  %s\nand the recording has\n  %s", sent, recorded)
	}
	return nil
}

// readdress rewrites a recorded agent message's identifier to the one this client
// used for the request it answers.
func (s *scriptedTransport) readdress(recorded []byte) []byte {
	var message map[string]any
	if err := json.Unmarshal(recorded, &message); err != nil {
		return recorded
	}
	recordedID, present := message["id"]
	if !present {
		return recorded // a notification
	}
	// A response carries no method; a request the agent makes does. Only a
	// response needs readdressing, because only it answers something this client
	// sent.
	if _, isRequest := message["method"]; isRequest {
		return recorded
	}
	ours, known := s.ourIDs[fmt.Sprint(recordedID)]
	if !known {
		return recorded
	}
	message["id"] = ours
	rewritten, err := json.Marshal(message)
	if err != nil {
		return recorded
	}
	return rewritten
}

func (s *scriptedTransport) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptedTransport) fail(err error) error {
	if s.failure == nil {
		s.failure = err
	}
	s.readyOnce.Do(func() { close(s.ready) })
	return s.failure
}

// explain prefers the script's own complaint to whatever the client made of the
// connection ending, because the first says what actually disagreed.
func (s *scriptedTransport) explain(err error) error {
	if scripted := s.err(); scripted != nil {
		return scripted
	}
	return err
}

func (s *scriptedTransport) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	if s.position < len(s.recorded.Messages) {
		return fmt.Errorf("the client stopped after %d of the recording's %d messages; the next was %s",
			s.position, len(s.recorded.Messages), s.recorded.Messages[s.position].Message)
	}
	return nil
}

// waitForCue blocks until the script has nothing more to feed, which is the point
// where the recording expects this side to act.
func (s *scriptedTransport) waitForCue() {
	<-s.ready
}

// interopObservations is what the replayed client's handlers saw, in the same
// shape the recorder wrote.
type interopObservations struct {
	mu sync.Mutex
	observedFields
}

type observedFields struct {
	Updates            []string
	PermissionRequests int
	StopReason         string
	FilesRead          []string
	FilesWritten       []string
	TerminalsCreated   int
	AuthRequired       bool
	Authenticated      bool
}

func (o *interopObservations) snapshot() observedFields {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.observedFields
}

// sameObservations compares what this client made of the exchange with what the
// recorder made of the same one.
//
// The stop reason is not compared here: the replay asserts it directly, and the
// recorder wrote it after the turn while this side's copy is filled in by the
// scenario rather than by a handler.
func sameObservations(got observedFields, want struct {
	Updates            []string `json:"updates"`
	PermissionRequests int      `json:"permissionRequests"`
	StopReason         string   `json:"stopReason"`
	FilesRead          []string `json:"filesRead"`
	FilesWritten       []string `json:"filesWritten"`
	TerminalsCreated   int      `json:"terminalsCreated"`
	AuthRequired       bool     `json:"authRequired"`
	Authenticated      bool     `json:"authenticated"`
},
) bool {
	return strings.Join(got.Updates, "|") == strings.Join(want.Updates, "|") &&
		got.PermissionRequests == want.PermissionRequests &&
		strings.Join(got.FilesRead, "|") == strings.Join(want.FilesRead, "|") &&
		strings.Join(got.FilesWritten, "|") == strings.Join(want.FilesWritten, "|") &&
		got.TerminalsCreated == want.TerminalsCreated &&
		got.AuthRequired == want.AuthRequired &&
		got.Authenticated == want.Authenticated
}

// interopClient is the client the recorder used, rebuilt here so that the replay
// exercises the same handlers. It is the same code in internal/cmd/interop; the
// duplication is deliberate, because a shared helper would let a change to the
// recorder silently change what the replay checks.
func interopClient(seen *interopObservations) (*acp.Client, error) {
	return acp.NewClient(&acp.ClientConfig{
		Info: &acp.Implementation{Name: "acp-go-interop", Version: "0.0.0"},
		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			seen.mu.Lock()
			seen.Updates = append(seen.Updates, describeInteropUpdate(notification.Update))
			seen.mu.Unlock()
		},
		RequestPermission: func(
			_ context.Context,
			request *acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			seen.mu.Lock()
			seen.PermissionRequests++
			seen.mu.Unlock()
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[0].OptionID},
			}, nil
		},
		ReadTextFile: func(
			_ context.Context,
			request *acp.ReadTextFileRequest,
		) (*acp.ReadTextFileResponse, error) {
			seen.mu.Lock()
			seen.FilesRead = append(seen.FilesRead, request.Path)
			seen.mu.Unlock()
			return &acp.ReadTextFileResponse{Content: "the file's contents"}, nil
		},
		WriteTextFile: func(
			_ context.Context,
			request *acp.WriteTextFileRequest,
		) (*acp.WriteTextFileResponse, error) {
			seen.mu.Lock()
			seen.FilesWritten = append(seen.FilesWritten, request.Path)
			seen.mu.Unlock()
			return &acp.WriteTextFileResponse{}, nil
		},
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				seen.mu.Lock()
				seen.TerminalsCreated++
				seen.mu.Unlock()
				return &acp.CreateTerminalResponse{TerminalID: "interop-terminal"}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{Output: "done\n", Truncated: false}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{ExitCode: acp.OptValue(uint32(0))}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(context.Context, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
}

func describeInteropUpdate(update acp.SessionUpdate) string {
	switch update := update.(type) {
	case *acp.AgentMessageChunk:
		if text, ok := update.Content.(*acp.TextContent); ok {
			return "agent_message_chunk:" + text.Text
		}
		return "agent_message_chunk"
	case *acp.ToolCall:
		return "tool_call:" + string(update.ToolCallID)
	case *acp.ToolCallUpdate:
		status, _ := update.Status.Get()
		return "tool_call_update:" + string(status)
	default:
		return fmt.Sprintf("%T", update)
	}
}
