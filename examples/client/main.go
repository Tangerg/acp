// Command client is a small but complete ACP client: it spawns an agent, runs
// one turn, and prints what the agent says while it works.
//
// A client owns the workspace and the user. This one owns a directory, reads and
// writes files inside it and nowhere else, and puts every permission request in
// front of whoever is at the terminal.
//
//	go run ./examples/client -prompt "remember the release is on Friday"
//
// By default it spawns the agent next door. Any other agent works too — the point
// of the protocol is that neither side knows which one it is talking to:
//
//	go run ./examples/client -prompt "hello" -- some-other-agent --flag
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Tangerg/acp"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := run(logger); err != nil {
		logger.Error("the turn did not finish", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// A library does not own operating-system signals; a program does.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prompt := flag.String("prompt", "", "what to ask the agent")
	directory := flag.String("cwd", ".", "the workspace the session runs in")
	flag.Parse()
	if *prompt == "" {
		flag.Usage()
		return errors.New("nothing to ask")
	}

	root, err := filepath.Abs(*directory)
	if err != nil {
		return err
	}
	place, openErr := openWorkspace(root)
	if openErr != nil {
		return openErr
	}
	defer func() { _ = place.root.Close() }()
	permissions := newPermissionPrompt(ctx, os.Stdin, os.Stdout)

	client, err := acp.NewClient(&acp.ClientConfig{
		Info:   &acp.Implementation{Name: "example-client", Version: "0.1.0"},
		Logger: logger,

		SessionUpdate:     printUpdate,
		RequestPermission: permissions.requestPermission,

		// Setting these is what advertises fs.readTextFile and fs.writeTextFile. An
		// agent may not call what this client did not advertise, and the connection
		// refuses it on both sides rather than reaching a handler that is not there.
		ReadTextFile:  place.readTextFile,
		WriteTextFile: place.writeTextFile,
	})
	if err != nil {
		return err
	}

	conn, err := client.Connect(ctx, agentTransport(ctx))
	if err != nil {
		return err
	}
	// Close ends the connection and the agent with it: the transport closes the
	// agent's stdin, waits, and only then signals.
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			logger.Error("stopping the agent failed", "error", closeErr)
		}
	}()

	if agent, named := conn.Negotiated().AgentInfo.Get(); named {
		fmt.Printf("connected to %s %s\n", agent.Name, agent.Version)
	}

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: root})
	if errors.Is(err, acp.ErrAuthRequired) {
		// A step in the lifecycle rather than a failure: conn.Negotiated().AuthMethods
		// says what this agent accepts, and conn.Authenticate answers it.
		return errors.New("this agent wants authentication, which this example does not do")
	}
	if err != nil {
		return err
	}

	// Interrupting ends the turn rather than the program. Cancelling the prompt's
	// context would only stop this side waiting; session/cancel is what obliges the
	// agent to stop and answer, and its answer is what proves it has.
	over := make(chan struct{})
	defer close(over)
	go func() {
		select {
		case <-over:
		case <-ctx.Done():
			fmt.Println("\ncancelling the turn...")
			if failed := session.Cancel(context.WithoutCancel(ctx), nil); failed != nil {
				logger.Error("cancelling failed", "error", failed)
			}
		}
	}()

	answer, err := session.Prompt(context.WithoutCancel(ctx), &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: *prompt}},
	})
	if err != nil {
		return err
	}
	fmt.Println("\nstop reason:", answer.StopReason)
	return nil
}

func agentTransport(ctx context.Context) acp.Transport {
	command := flag.Args()
	if len(command) == 0 {
		command = []string{"go", "run", "./examples/agent"}
	}

	// Deliberately not this program's interrupt context: an interrupt ends the
	// turn, and killing the agent underneath it would take away the answer that
	// says the turn ended. Shutting the agent down is Close's job.
	//nolint:gosec // The command is this program's own arguments; running it is what the program is for.
	agent := exec.CommandContext(context.WithoutCancel(ctx), command[0], command[1:]...)
	// The agent's diagnostics belong on this terminal. Its stdout does not: that is
	// the protocol stream, and the transport owns both pipes.
	agent.Stderr = os.Stderr

	return acp.NewCommandTransport(&acp.CommandConfig{Command: agent})
}

// The agent's running commentary. Rendering is a client's whole job on this side,
// so this is the switch a real one grows into a user interface.
func printUpdate(_ context.Context, _ *acp.ClientConn, notification *acp.SessionNotification) {
	switch update := notification.Update.(type) {
	case *acp.AgentMessageChunk:
		if text, ok := update.Content.(*acp.TextContent); ok {
			fmt.Print(text.Text)
		}
	case *acp.AgentThoughtChunk:
		if text, ok := update.Content.(*acp.TextContent); ok {
			fmt.Printf("\033[2m%s\033[0m", text.Text)
		}
	case *acp.ToolCall:
		fmt.Printf("\n  [%s] %s\n", update.ToolCallID, update.Title)
	case *acp.ToolCallUpdate:
		if status, reported := update.Status.Get(); reported {
			fmt.Printf("  [%s] %s\n", update.ToolCallID, status)
		}
	case *acp.Plan:
		for _, entry := range update.Entries {
			fmt.Printf("  - %s (%s)\n", entry.Content, entry.Status)
		}
	}
}
