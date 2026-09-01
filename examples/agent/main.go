// Command agent is a small but complete ACP agent, and the skeleton a real one
// starts from.
//
// It speaks the protocol over its own stdin and stdout, which is how a client
// runs an agent: the client spawns the process and owns both pipes. Given a
// prompt it streams a reply, asks permission before touching the workspace, and
// appends what it was told to NOTES.md in the session's working directory.
//
// Run it through the client next door, which spawns it:
//
//	go run ./examples/client -prompt "remember the release is on Friday"
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/Tangerg/acp"
)

func main() {
	// Diagnostics go to stderr, always. Stdout is the protocol stream, so a logger
	// pointed at it would corrupt every connection it was meant to help debug.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("the agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// A library does not own operating-system signals; a program does.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	open := &sessions{directories: make(map[acp.SessionID]string)}
	agent, err := acp.NewAgent(&acp.AgentConfig{
		Info:   &acp.Implementation{Name: "example-agent", Version: "0.1.0"},
		Logger: logger,

		NewSession: open.newSession,
		Prompt:     open.prompt,
		Cancel: func(_ context.Context, session *acp.AgentSession, _ *acp.CancelNotification) {
			// The turn's context is already cancelled by the time this runs, so an
			// agent whose work descends from it has nothing more to do here.
			logger.Info("turn cancelled", "session", session.ID())
		},
	})
	if err != nil {
		return err
	}

	// Run serves one connection until the client hangs up or the process is
	// signalled. A clean end of stream is not a failure, and neither is the signal.
	if err := agent.Run(ctx, acp.NewStdioTransport()); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// What this agent remembers between calls. A session identifier is the agent's to
// mint and its meaning is the agent's to keep; the connection only routes by it.
type sessions struct {
	mu          sync.Mutex
	count       int
	directories map[acp.SessionID]string
}

func (s *sessions) newSession(_ context.Context, request *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.count++
	id := acp.SessionID(fmt.Sprintf("session-%d", s.count))
	s.directories[id] = request.Cwd
	return &acp.NewSessionResponse{SessionID: id}, nil
}

func (s *sessions) directory(id acp.SessionID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cwd, known := s.directories[id]
	return cwd, known
}

func (s *sessions) prompt(
	ctx context.Context,
	session *acp.AgentSession,
	request *acp.PromptRequest,
) (*acp.PromptResponse, error) {
	cwd, known := s.directory(session.ID())
	if !known {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "no such session"}
	}
	note := promptText(request)
	notes := filepath.Join(cwd, "NOTES.md")

	if err := say(ctx, session, "I will append that to "+notes+"."); err != nil {
		return nil, err
	}

	// What the client advertised at initialize is what this agent may do. Asking
	// first is not politeness: the call would be refused, and this way the user
	// hears why in the turn rather than seeing an error.
	if !session.Conn().Peer().ClientCapabilities.Fs.WriteTextFile {
		if err := say(ctx, session, "This client does not let me write files, so I cannot."); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	decision, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{
			ToolCallID: acp.ToolCallID("write-" + string(session.ID())),
			Title:      acp.OptValue("append a note to " + notes),
			Kind:       acp.OptValue(acp.ToolKindEdit),
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Name: "Append it", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: "reject", Name: "Leave it alone", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return nil, err
	}
	selected, chosen := decision.Outcome.(*acp.SelectedPermissionOutcome)
	if !chosen {
		// The other outcome is cancelled, which means the turn ended rather than
		// that the answer was no.
		return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if selected.OptionID != "allow" {
		if err := say(ctx, session, "Left it alone."); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	if err := appendNote(ctx, session, notes, note); err != nil {
		return nil, err
	}
	if err := say(ctx, session, "Done."); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func appendNote(ctx context.Context, session *acp.AgentSession, path, note string) error {
	var existing string
	if session.Conn().Peer().ClientCapabilities.Fs.ReadTextFile {
		// A missing file is the ordinary case on the first note, and the client
		// reports it as an error like any other. Reading and writing are two
		// capabilities, so a client may allow one and refuse the other.
		if read, err := session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: path}); err == nil {
			existing = read.Content
		}
	}
	_, err := session.WriteTextFile(ctx, &acp.WriteTextFileParams{
		Path:    path,
		Content: existing + "- " + note + "\n",
	})
	return err
}

func say(ctx context.Context, session *acp.AgentSession, text string) error {
	return session.Update(ctx, &acp.SessionUpdateParams{
		Update: &acp.AgentMessageChunk{ContentChunk: acp.ContentChunk{
			Content: &acp.TextContent{Text: text},
		}},
	})
}

// Text is the one content type every agent must accept. The rest — images, audio,
// embedded resources — are optional and advertised through promptCapabilities, so
// an agent that ignores them must not pretend they arrived.
func promptText(request *acp.PromptRequest) string {
	var parts []string
	for _, block := range request.Prompt {
		if text, ok := block.(*acp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}
