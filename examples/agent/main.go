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
	"log/slog"
	"os"
	"os/signal"
	"strings"
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
	// A stdio agent ends when its client closes stdin. A terminal interrupt is
	// the client's to handle, by cancelling the turn: it reaches this process too
	// when both share the terminal, and must not take the answer away.
	signal.Ignore(os.Interrupt)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	open := newSessions()
	agent, err := acp.NewAgent(&acp.AgentConfig{
		Info:   &acp.Implementation{Name: "example-agent", Version: "0.1.0"},
		Logger: logger,

		NewSession: open.newSession,
		Prompt:     open.prompt,
	})
	if err != nil {
		return err
	}

	// Run serves one connection until the client hangs up or the process is
	// asked to stop. Neither is a failure.
	if runErr := agent.Run(ctx, acp.NewStdioTransport()); runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

func say(ctx context.Context, session *acp.AgentSession, text string) error {
	return session.Update(ctx, &acp.SessionUpdateParams{
		Update: &acp.AgentMessageChunk{
			Content: &acp.TextContent{Text: text},
		},
	})
}

// Text and resource links are baseline prompt content. A link remains a
// reference: its URI alone does not authorize reading a file or fetching a URL.
func promptText(request *acp.PromptRequest) string {
	var parts []string
	for _, block := range request.Prompt {
		switch content := block.(type) {
		case *acp.TextContent:
			parts = append(parts, content.Text)
		case *acp.ResourceLink:
			parts = append(parts, content.Name+" ("+content.URI+")")
		}
	}
	return strings.Join(parts, " ")
}
