package acp_test

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/acp"
)

// A whole turn over newline-delimited JSON, which is the transport a local
// deployment actually uses.
//
// The in-memory transport carries messages rather than bytes on purpose — an
// in-memory transport that encoded and re-decoded would be testing the codec a
// second time and hiding a framing bug rather than exposing one. So the framing
// is tested here, and it is tested by running a turn through it rather than by
// asserting on bytes: a framer whose answer depends on where a read splits is the
// defect this exists to catch.
func TestATurnOverNewlineDelimitedJSON(t *testing.T) {
	// Two pipes crossed over, which is what a subprocess's stdin and stdout are.
	clientReads, agentWrites := io.Pipe()
	agentReads, clientWrites := io.Pipe()

	clientSide := acp.NewIOTransport(clientReads, clientWrites)
	agentSide := acp.NewIOTransport(agentReads, agentWrites)

	updates := make(chan string, 4)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			if chunk, ok := notification.Update.(*acp.AgentMessageChunk); ok {
				if text, ok := chunk.Content.(*acp.TextContent); ok {
					updates <- text.Text
				}
			}
		},
		RequestPermission: denyingPermission,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, request *acp.PromptRequest) (*acp.PromptResponse, error) {
		// A message with a newline and a quote in it, because those are what
		// framing gets wrong: a newline inside a string is not a message boundary.
		text, ok := request.Prompt[0].(*acp.TextContent)
		if !ok {
			return nil, errors.New("the prompt did not survive the wire")
		}
		if updateErr := session.Update(ctx, &acp.SessionUpdateParams{
			Update: &acp.AgentMessageChunk{
				ContentChunk: acp.ContentChunk{Content: &acp.TextContent{Text: "echo: " + text.Text}},
			},
		}); updateErr != nil {
			return nil, updateErr
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	ctx := context.Background()
	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	defer agentConn.Close() //nolint:errcheck // idempotent.

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	const awkward = "a line\nand \"another\", with a tab\there"
	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: awkward}},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Errorf("stop reason = %q", response.StopReason)
	}
	if echoed := <-updates; echoed != "echo: "+awkward {
		t.Errorf("the text did not survive the wire: %q", echoed)
	}
}

// Closing a byte-stream connection unblocks the read that is parked on it, which
// is the one thing the Connection contract insists Close does.
func TestClosingAByteStreamUnblocksItsRead(t *testing.T) {
	reader, writer := io.Pipe()
	transport := acp.NewIOTransport(reader, writer)

	stream, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	parked := make(chan error, 1)
	go func() {
		_, err := stream.Read(context.Background())
		parked <- err
	}()

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-parked; err == nil {
		t.Fatal("the parked Read returned no error, so it was not unblocked by Close")
	}

	// And Close is idempotent.
	if err := stream.Close(); err != nil {
		t.Fatalf("the second Close reported %v", err)
	}
	// And a transport is connected at most once.
	if _, err := transport.Connect(context.Background()); err == nil {
		t.Fatal("the transport was connected twice")
	}
}

// A stream that is not the protocol is a failure the reader reports, not one it
// tries to recover from. Blank lines are the exception: skipping them keeps a
// stream somebody pretty-printed readable rather than fatal.
func TestByteStreamFramingRefusesWhatIsNotAMessage(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		refused bool
	}{
		{name: "a blank line before a message", stream: "\n\n{\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n"},
		{name: "a message with no trailing newline", stream: `{"jsonrpc":"2.0","method":"x"}`},
		{name: "not JSON at all", stream: "hello\n", refused: true},
		{name: "JSON that is not a message", stream: "[1,2,3]\n", refused: true},
		{name: "the wrong protocol version", stream: `{"jsonrpc":"1.0","method":"x"}` + "\n", refused: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, err := acp.NewIOTransport(
				io.NopCloser(strings.NewReader(test.stream)),
				nopWriteCloser{io.Discard},
			).Connect(context.Background())
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			defer stream.Close() //nolint:errcheck // idempotent.

			_, err = stream.Read(context.Background())
			if test.refused && err == nil {
				t.Fatal("the reader accepted something that is not a message")
			}
			if !test.refused && err != nil {
				t.Fatalf("the reader refused a message: %v", err)
			}
		})
	}
}

// Constructors retain configuration for Connect, so invalid zero values must be
// reported at that boundary rather than dereferenced in a read, write, or process
// setup path. Only a zero grace has default meaning; a negative duration is a
// configuration error rather than a second spelling of the default.
func TestTransportConfigurationErrorsDoNotPanic(t *testing.T) {
	tests := map[string]acp.Transport{
		"nil IO reader":      acp.NewIOTransport(nil, nopWriteCloser{io.Discard}),
		"nil IO writer":      acp.NewIOTransport(io.NopCloser(strings.NewReader("")), nil),
		"nil command config": acp.NewCommandTransport(nil),
		"nil command":        acp.NewCommandTransport(&acp.CommandConfig{}),
		"negative grace": acp.NewCommandTransport(&acp.CommandConfig{
			Command:          exec.CommandContext(context.Background(), "true"),
			TerminationGrace: -time.Second,
		}),
	}
	for name, transport := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := transport.Connect(context.Background()); err == nil {
				t.Fatal("Connect accepted an invalid transport configuration")
			}
		})
	}
}

func TestByteStreamTransportsDoNotOpenAfterSetupCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := exec.CommandContext(context.Background(), "true")
	for name, transport := range map[string]acp.Transport{
		"IO": acp.NewIOTransport(
			io.NopCloser(strings.NewReader("")),
			nopWriteCloser{io.Discard},
		),
		"command": acp.NewCommandTransport(&acp.CommandConfig{Command: command}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := transport.Connect(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Connect returned %v, want context.Canceled", err)
			}
		})
	}
	if command.Process != nil {
		t.Fatal("the command was started after its setup context was cancelled")
	}
}

// The command transport starts a process and reaps it. The agent here is a
// program that answers initialize and nothing else, which is enough to prove the
// pipes are wired to the right ends.
func TestTheCommandTransportStartsAndReapsAnAgent(t *testing.T) {
	// A minimal agent: read one line, answer it, exit. Written as a shell command
	// so that the test needs no second binary.
	const script = `read -r line; printf '%s\n' ` +
		`'{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'`

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	transport := acp.NewCommandTransport(&acp.CommandConfig{Command: cmd})

	conn, err := testClient(t).Connect(ctx, transport)
	if err != nil {
		t.Fatalf("Client.Connect over a subprocess: %v", err)
	}
	if version := conn.Peer().ProtocolVersion; version != 1 {
		t.Errorf("negotiated version %d, want 1", version)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Wait(); err != nil {
		t.Errorf("Wait reported %v, want nil after a local Close", err)
	}
	if _, err := transport.Connect(ctx); err == nil {
		t.Error("the transport was connected twice")
	}
}

// An agent that takes the hint is reaped by the first wait, and closing costs
// nothing.
//
// Closing the pipes is how a client says no more requests are coming. This agent
// reads until end of input and exits, which is what an agent should do.
func TestAnAgentThatExitsOnEndOfInputIsReapedAtOnce(t *testing.T) {
	// A grace far longer than the test's own patience: if this sequence ever needs
	// the grace at all, the test fails on its deadline rather than passing slowly.
	const grace = time.Hour

	stream := startAgentScript(t, `while read -r line; do :; done`, grace)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// And it is idempotent, which matters more here than elsewhere: a process can
	// only be reaped once.
	if err := stream.Close(); err != nil {
		t.Fatalf("the second Close reported %v", err)
	}
}

// An agent that takes no hint at all is still gone when Close returns.
//
// This one ignores end of input and ignores being asked to stop, which is what a
// wedged agent looks like. Close used to wait on it for ever, and with it every
// caller of ClientConn.Close. Now each step has a deadline and the last step is not
// a request.
func TestAnAgentThatIgnoresEveryHintIsKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a POSIX shell script, and Windows has no signal to ignore")
	}
	// Short, because the test spends two of these on purpose. Nothing here sleeps
	// for a fixed time: the grace is the bound on how long a step may take, and the
	// steps end as soon as the process does.
	const grace = 50 * time.Millisecond

	// Ignores SIGTERM, and never reads its stdin, so end of input tells it nothing.
	stream := startAgentScript(t, `trap "" TERM; while :; do sleep 0.01; done`, grace)

	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return; a wedged agent still owns the client that started it")
	}
}

// startAgentScript runs a shell script as the agent and returns the connection to
// it. The script is the agent because the test needs no second binary.
func startAgentScript(t *testing.T, script string, grace time.Duration) acp.Connection {
	t.Helper()

	transport := acp.NewCommandTransport(&acp.CommandConfig{
		//nolint:gosec // the script is this file's own, and running one is the point.
		Command:          exec.CommandContext(context.Background(), "sh", "-c", script),
		TerminationGrace: grace,
	})
	stream, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// The stdio transport is the agent's side of a local connection, and it is this
// process's own streams.
//
// The test is shallow on purpose. Connecting it would take os.Stdin and os.Stdout,
// and closing that connection would close them — which would take the test binary
// with it. What can be checked is that the constructor is the one thing it claims
// to be: a transport, built without reaching into the globals from somewhere a
// reader cannot see.
func TestTheStdioTransportIsAvailable(t *testing.T) {
	if acp.NewStdioTransport() == nil {
		t.Fatal("NewStdioTransport returned nothing")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
