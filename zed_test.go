package acp_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Tangerg/acp"
)

// The evidence from the other direction: a real editor driving an agent built on
// this module.
//
// testdata/interop is this module's client against the reference implementation's
// agent. This is the mirror of it, and the reference implementation cannot
// provide it — the client half of ACP is an editor, and an editor is a person
// clicking. So these transcripts were recorded by hand, once, with Zed spawning
// an agent built on this package: the wire is real, the command ran in the
// editor's terminal, and the cancellation came from its stop button.
//
// Being hand-recorded is the cost, and it is why this is a corpus rather than a
// gate that could re-run: nothing here can catch Zed changing. What it does catch
// is this package changing — a decoder that starts dropping a property another
// implementation actually sends, or an encoder that stops being able to say what
// it said. CONTRIBUTING.md has the recording procedure.

type zedTranscript struct {
	Scenario   string `json:"scenario"`
	Provenance struct {
		Client        string `json:"client"`
		ClientVersion string `json:"clientVersion"`
		Recorded      string `json:"recorded"`
	} `json:"provenance"`
	ClientToAgent []json.RawMessage `json:"clientToAgent"`
	AgentToClient []json.RawMessage `json:"agentToClient"`
}

func TestTheBytesARealEditorSent(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "zed", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no recorded editor transcripts")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			//nolint:gosec // the path is a glob result under testdata.
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var transcript zedTranscript
			if err := json.Unmarshal(data, &transcript); err != nil {
				t.Fatal(err)
			}
			if transcript.Provenance.Client == "" || transcript.Provenance.ClientVersion == "" {
				t.Fatal("a transcript without provenance is an anecdote")
			}

			methods := map[string]string{}
			checked := 0
			for _, message := range append(transcript.ClientToAgent, transcript.AgentToClient...) {
				envelope := envelopeOf(t, message)
				if envelope.Method != "" {
					methods[string(envelope.ID)] = envelope.Method
				}
			}

			// The editor's own bytes have to survive this package's types
			// unchanged. A property it sends and this package does not know about
			// would be dropped here, silently, exactly as it would be in
			// production.
			for _, message := range transcript.ClientToAgent {
				checked += replayZedMessage(t, message, methods, true)
			}
			// The agent's are held to the weaker promise, because a schema
			// property with a declared default is allowed to come back spelled
			// either way: agentCapabilities.authMethods defaults to the empty
			// list, so an answer that omits it and one that sends [] are the same
			// answer. What must hold is that encoding a decoded message is stable.
			for _, message := range transcript.AgentToClient {
				checked += replayZedMessage(t, message, methods, false)
			}
			if checked == 0 {
				t.Fatal("nothing in this transcript was checked")
			}
			t.Logf("%s: %d payloads from %s %s",
				transcript.Scenario, checked,
				transcript.Provenance.Client, transcript.Provenance.ClientVersion)
		})
	}
}

// The turn the editor stopped is the one worth asserting about, because the stop
// reason is this package's obligation rather than the application's: the handler
// behind this transcript returned its context's error.
func TestTheEditorsCancellationBecameTheCancelledStopReason(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "zed", "terminal-and-cancellation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript zedTranscript
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatal(err)
	}

	cancelled := false
	for _, message := range transcript.ClientToAgent {
		if envelopeOf(t, message).Method == "session/cancel" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatal("the editor never sent session/cancel")
	}

	var reasons []acp.StopReason
	for _, message := range transcript.AgentToClient {
		envelope := envelopeOf(t, message)
		if len(envelope.Result) == 0 {
			continue
		}
		var response acp.PromptResponse
		if err := json.Unmarshal(envelope.Result, &response); err == nil && response.StopReason != "" {
			reasons = append(reasons, response.StopReason)
		}
	}
	if len(reasons) != 2 || reasons[0] != acp.StopReasonEndTurn || reasons[1] != acp.StopReasonCancelled {
		t.Fatalf("the two turns ended with %v, want [end_turn cancelled]", reasons)
	}

	// And the order the cancelled turn ended in, which is the part a client
	// depends on: the agent's last word went out after the cancellation and
	// before the answer, so a client that stopped listening at the cancellation
	// would have lost it.
	last := transcript.AgentToClient[len(transcript.AgentToClient)-1]
	if len(envelopeOf(t, last).Result) == 0 {
		t.Fatalf("the last thing the agent sent was not the answer: %s", last)
	}
	previous := envelopeOf(t, transcript.AgentToClient[len(transcript.AgentToClient)-2])
	if previous.Method != "session/update" {
		t.Fatalf("the message before the cancelled answer was %q, want session/update", previous.Method)
	}
}

// replayZedMessage reports how many payloads it checked, which is zero for a
// method this test has no type mapping for.
func replayZedMessage(t *testing.T, message json.RawMessage, methods map[string]string, exact bool) int {
	t.Helper()

	envelope := envelopeOf(t, message)
	method, kind, payload := envelope.Method, "params", envelope.Params
	if method == "" {
		method, kind, payload = methods[string(envelope.ID)], "result", envelope.Result
	}
	if len(payload) == 0 {
		return 0
	}
	replay, known := zedReplays[method+" "+kind]
	if !known {
		t.Errorf("%s %s is in the transcript and this test has no type for it", method, kind)
		return 0
	}

	once, err := replay(payload)
	if err != nil {
		t.Errorf("%s %s: %v", method, kind, err)
		return 1
	}
	twice, err := replay(once)
	if err != nil {
		t.Errorf("%s %s, re-encoded: %v", method, kind, err)
		return 1
	}
	if !equalJSON(once, twice) {
		t.Errorf("%s %s does not normalise to a fixed point:\n once: %s\ntwice: %s", method, kind, once, twice)
	}
	if exact && !equalJSON(payload, once) {
		t.Errorf("%s %s did not survive this package's types:\n sent: %s\n kept: %s", method, kind, payload, once)
	}
	return 1
}

// The transcript records each direction in the order it was written, so a
// response is matched to its method through the identifier its request used.
var zedReplays = map[string]func(json.RawMessage) (json.RawMessage, error){
	"initialize params":                 replayAs[acp.InitializeRequest],
	"initialize result":                 replayAs[acp.InitializeResponse],
	"session/new params":                replayAs[acp.NewSessionRequest],
	"session/new result":                replayAs[acp.NewSessionResponse],
	"session/prompt params":             replayAs[acp.PromptRequest],
	"session/prompt result":             replayAs[acp.PromptResponse],
	"session/cancel params":             replayAs[acp.CancelNotification],
	"session/update params":             replayAs[acp.SessionNotification],
	"terminal/create params":            replayAs[acp.CreateTerminalRequest],
	"terminal/create result":            replayAs[acp.CreateTerminalResponse],
	"terminal/output params":            replayAs[acp.TerminalOutputRequest],
	"terminal/output result":            replayAs[acp.TerminalOutputResponse],
	"terminal/wait_for_exit params":     replayAs[acp.WaitForTerminalExitRequest],
	"terminal/wait_for_exit result":     replayAs[acp.WaitForTerminalExitResponse],
	"terminal/kill params":              replayAs[acp.KillTerminalRequest],
	"terminal/kill result":              replayAs[acp.KillTerminalResponse],
	"terminal/release params":           replayAs[acp.ReleaseTerminalRequest],
	"terminal/release result":           replayAs[acp.ReleaseTerminalResponse],
	"session/request_permission params": replayAs[acp.RequestPermissionRequest],
	"session/request_permission result": replayAs[acp.RequestPermissionResponse],
	"fs/read_text_file params":          replayAs[acp.ReadTextFileRequest],
	"fs/read_text_file result":          replayAs[acp.ReadTextFileResponse],
	"fs/write_text_file params":         replayAs[acp.WriteTextFileRequest],
	"fs/write_text_file result":         replayAs[acp.WriteTextFileResponse],
}

func replayAs[T any](raw json.RawMessage) (json.RawMessage, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// A zedMessage is as much of a JSON-RPC envelope as replay needs.
type zedMessage struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func envelopeOf(t *testing.T, message json.RawMessage) zedMessage {
	t.Helper()
	var envelope zedMessage
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("a recorded message is not JSON-RPC: %v", err)
	}
	return envelope
}

func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
