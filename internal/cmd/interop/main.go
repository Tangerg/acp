// Command interop drives this module's client against an agent built on the
// TypeScript SDK, and records what crossed the wire.
//
// Two Go endpoints talking to each other share any wire bug they have, so they
// are not release evidence. This is the evidence: a real subprocess, speaking
// newline-delimited JSON, built on the reference implementation.
//
// It is not run by go test. scripts/interop.sh runs it against a pinned SDK
// checkout and commits the transcripts; go test replays those with no network and
// no Node, exactly as the fixture corpus works. The recorded bytes are the
// reference implementation's, so replaying them is still checking this package
// against another implementation rather than against itself.
//
// Usage:
//
//	go run ./internal/cmd/interop -dir testdata/interop -- node_modules/.bin/tsx interop-agent.ts
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

func main() {
	dir := flag.String("dir", filepath.Join("testdata", "interop"), "where to write the transcripts")
	sdkCommit := flag.String("sdk-commit", "", "the SDK commit the agent was built from")
	sdkVersion := flag.String("sdk-version", "", "the SDK package version")
	schemaTag := flag.String("schema", "", "the schema release the SDK was generated from")
	flag.Parse()

	// The agent is the positional arguments rather than one string to split,
	// because a temporary directory's path may contain a space and splitting on
	// whitespace would find it eventually.
	agentCommand := flag.Args()
	if len(agentCommand) == 0 {
		fmt.Fprintln(os.Stderr, "interop: the agent command is the positional arguments, after --")
		os.Exit(2)
	}
	provenance := Provenance{Commit: *sdkCommit, Version: *sdkVersion, Schema: *schemaTag}

	if err := run(agentCommand, *dir, provenance); err != nil {
		fmt.Fprintf(os.Stderr, "interop: %v\n", err)
		os.Exit(1)
	}
}

// A Provenance is what the transcripts were produced against. Without it "the two
// SDKs agree" is an anecdote rather than release evidence.
type Provenance struct {
	Commit  string `json:"sdkCommit"`
	Version string `json:"sdkVersion"`
	Schema  string `json:"schemaRelease"`
}

func run(agentCommand []string, dir string, provenance Provenance) error {
	scenarios := []struct {
		name string
		file string
		play func(context.Context, *acp.ClientConn, *observations) error
	}{
		{name: "a full turn with a permission prompt", file: "turn.json", play: playTurn},
		{name: "a cancelled turn whose final updates still arrive", file: "cancel.json", play: playCancel},
		{name: "authentication as control flow", file: "auth.json", play: playAuth},
		{name: "the workspace operations", file: "workspace.json", play: playWorkspace},
		{name: "both elicitation modes", file: "elicitation.json", play: playElicitation},
	}

	for _, scenario := range scenarios {
		transcript, err := record(agentCommand, scenario.name, provenance, scenario.play)
		if err != nil {
			return fmt.Errorf("%s: %w", scenario.name, err)
		}
		encoded, err := json.MarshalIndent(transcript, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, scenario.file)
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Printf("interop: %s → %s (%d messages)\n", scenario.name, path, len(transcript.Messages))
	}
	return nil
}

// record runs one scenario and returns what crossed the wire.
func record(
	agentCommand []string,
	name string,
	provenance Provenance,
	play func(context.Context, *acp.ClientConn, *observations) error,
) (*Transcript, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seen := &observations{}
	client, err := interopClient(seen)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, agentCommand[0], agentCommand[1:]...) //nolint:gosec // the command is this tool's own argument.
	cmd.Stderr = os.Stderr

	recorder := &recordingTransport{inner: acp.NewCommandTransport(&acp.CommandConfig{Command: cmd})}
	conn, err := client.Connect(ctx, recorder)
	if err != nil {
		return nil, fmt.Errorf("connecting to the reference agent: %w", err)
	}

	playErr := play(ctx, conn, seen)
	closeErr := conn.Close()
	if playErr != nil {
		return nil, playErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return &Transcript{
		Comment: "Recorded by internal/cmd/interop against an agent built on the reference " +
			"implementation. Written by scripts/interop.sh; replayed by interop_test.go. Do not edit.",
		Scenario:   name,
		Provenance: provenance,
		Observed:   seen.snapshot(),
		Messages:   recorder.messages(),
	}, nil
}

// A Transcript is one scenario's wire log, plus what the client observed while it
// happened.
//
// The observations are the point of recording anything: the messages prove the
// two implementations exchanged what they exchanged, and these prove this package
// made the right thing of them.
type Transcript struct {
	Comment    string     `json:"$comment"`
	Scenario   string     `json:"scenario"`
	Provenance Provenance `json:"provenance"`
	Observed   Observed   `json:"observed"`
	Messages   []Recorded `json:"messages"`
}

// A Recorded is one message, and which side sent it.
type Recorded struct {
	From    string          `json:"from"`
	Message json.RawMessage `json:"message"`
}

// Observed is what the client's handlers saw.
type Observed struct {
	Updates            []string `json:"updates"`
	PermissionRequests int      `json:"permissionRequests"`
	StopReason         string   `json:"stopReason"`
	FilesRead          []string `json:"filesRead"`
	FilesWritten       []string `json:"filesWritten"`
	TerminalsCreated   int      `json:"terminalsCreated"`
	Elicitations       []string `json:"elicitations,omitempty"`
	ElicitationsDone   []string `json:"elicitationsDone,omitempty"`
	AuthRequired       bool     `json:"authRequired"`
	Authenticated      bool     `json:"authenticated"`
}

type observations struct {
	mu sync.Mutex
	Observed
}

func (o *observations) update(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Updates = append(o.Updates, text)
}

func (o *observations) snapshot() Observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Observed
}

func interopClient(seen *observations) (*acp.Client, error) {
	return acp.NewClient(&acp.ClientConfig{
		Info: &acp.Implementation{Name: "acp-go-interop", Version: "0.0.0"},
		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			seen.update(describeUpdate(notification.Update))
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
		// Both modes, because the capability the client advertises is what decides
		// whether the reference implementation may send either.
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context,
				request *acp.CreateElicitationRequest,
				mode *acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				seen.mu.Lock()
				seen.Elicitations = append(seen.Elicitations,
					"form:"+describeScope(mode.Value)+":"+request.Message)
				seen.mu.Unlock()
				answer := acp.ElicitationContentValueString("main")
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{
						Content: acp.OptValue(map[string]acp.ElicitationContentValue{"branch": &answer}),
					},
				}, nil
			},
			URL: func(
				_ context.Context,
				_ *acp.CreateElicitationRequest,
				mode *acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				seen.mu.Lock()
				seen.Elicitations = append(seen.Elicitations, "url:"+string(mode.ElicitationID))
				seen.mu.Unlock()
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{},
				}, nil
			},
			Complete: func(_ context.Context, notification *acp.CompleteElicitationNotification) {
				seen.mu.Lock()
				seen.ElicitationsDone = append(seen.ElicitationsDone, string(notification.ElicitationID))
				seen.mu.Unlock()
			},
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
			Create: func(
				_ context.Context,
				_ *acp.CreateTerminalRequest,
			) (*acp.CreateTerminalResponse, error) {
				seen.mu.Lock()
				seen.TerminalsCreated++
				seen.mu.Unlock()
				return &acp.CreateTerminalResponse{TerminalID: "interop-terminal"}, nil
			},
			Output: func(
				_ context.Context,
				_ *acp.TerminalOutputRequest,
			) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{Output: "done\n", Truncated: false}, nil
			},
			WaitForExit: func(
				_ context.Context,
				_ *acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{ExitCode: acp.OptValue(uint32(0))}, nil
			},
			Kill: func(_ context.Context, _ *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(_ context.Context, _ *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
}

// describeUpdate reduces an update to something a transcript can assert on. The
// arm and its salient field, not the whole payload: the messages are already in
// the transcript, and what this records is what the client made of them.
func describeUpdate(update acp.SessionUpdate) string {
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

func playTurn(ctx context.Context, conn *acp.ClientConn, seen *observations) error {
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		return err
	}
	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "turn"}},
	})
	if err != nil {
		return err
	}
	seen.mu.Lock()
	seen.StopReason = string(response.StopReason)
	seen.mu.Unlock()
	return nil
}

func playCancel(ctx context.Context, conn *acp.ClientConn, seen *observations) error {
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		return err
	}

	prompted := make(chan error, 1)
	var response *acp.PromptResponse
	go func() {
		var err error
		response, err = session.Prompt(ctx, &acp.PromptParams{
			Prompt: []acp.ContentBlock{&acp.TextContent{Text: "cancel"}},
		})
		prompted <- err
	}()

	// Wait for the first update, which is how this side knows the turn is under
	// way rather than sleeping and hoping.
	for range 600 {
		seen.mu.Lock()
		started := len(seen.Updates) > 0
		seen.mu.Unlock()
		if started {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := session.Cancel(ctx, nil); err != nil {
		return err
	}
	if err := <-prompted; err != nil {
		return err
	}

	seen.mu.Lock()
	seen.StopReason = string(response.StopReason)
	seen.mu.Unlock()
	return nil
}

func playAuth(ctx context.Context, conn *acp.ClientConn, seen *observations) error {
	_, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/auth"})
	if !errors.Is(err, acp.ErrAuthRequired) {
		return fmt.Errorf("the agent answered session/new with an error that does not match ErrAuthRequired: %w", err)
	}
	seen.mu.Lock()
	seen.AuthRequired = true
	seen.mu.Unlock()

	if _, err := conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "interop"}); err != nil {
		return err
	}
	if _, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/auth"}); err != nil {
		return err
	}
	seen.mu.Lock()
	seen.Authenticated = true
	seen.mu.Unlock()
	return nil
}

func playWorkspace(ctx context.Context, conn *acp.ClientConn, seen *observations) error {
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		return err
	}
	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "workspace"}},
	})
	if err != nil {
		return err
	}
	seen.mu.Lock()
	seen.StopReason = string(response.StopReason)
	seen.mu.Unlock()
	return nil
}

func playElicitation(ctx context.Context, conn *acp.ClientConn, seen *observations) error {
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		return err
	}
	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "elicit"}},
	})
	if err != nil {
		return err
	}
	seen.mu.Lock()
	seen.StopReason = string(response.StopReason)
	seen.mu.Unlock()
	return nil
}

// describeScope names which of the two scopes an elicitation arrived under,
// without naming the identifier that distinguishes them: a request scope carries
// a JSON-RPC id, and this API does not hand those out.
func describeScope(scope acp.ElicitationFormModeValue) string {
	switch scope := scope.(type) {
	case *acp.ElicitationFormModeSession:
		if id, tied := scope.ToolCallID.Get(); tied {
			return "session/" + string(id)
		}
		return "session"
	case *acp.ElicitationFormModeRequest:
		return "request"
	default:
		return "unknown"
	}
}

// A recordingTransport logs every message in both directions.
type recordingTransport struct {
	inner acp.Transport

	mu  sync.Mutex
	log []Recorded
}

func (t *recordingTransport) Connect(ctx context.Context) (acp.Connection, error) {
	stream, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingConnection{inner: stream, transport: t}, nil
}

func (t *recordingTransport) record(from string, message jsonrpc.Message) {
	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.log = append(t.log, Recorded{From: from, Message: encoded})
}

func (t *recordingTransport) messages() []Recorded {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.log
}

type recordingConnection struct {
	inner     acp.Connection
	transport *recordingTransport
}

func (c *recordingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.inner.Read(ctx)
	if err != nil {
		return nil, err
	}
	c.transport.record("agent", message)
	return message, nil
}

func (c *recordingConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	c.transport.record("client", message)
	return c.inner.Write(ctx, message)
}

func (c *recordingConnection) Close() error { return c.inner.Close() }
