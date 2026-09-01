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
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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
	place := &workspace{root: root, input: bufio.NewReader(os.Stdin)}

	client, err := acp.NewClient(&acp.ClientConfig{
		Info:   &acp.Implementation{Name: "example-client", Version: "0.1.0"},
		Logger: logger,

		SessionUpdate:     printUpdate,
		RequestPermission: place.requestPermission,

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
	defer func() { _ = conn.Close() }()

	if agent, named := conn.Peer().AgentInfo.Get(); named {
		fmt.Printf("connected to %s %s\n", agent.Name, agent.Version)
	}

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: root, McpServers: []acp.McpServer{}})
	if errors.Is(err, acp.ErrAuthRequired) {
		// A step in the lifecycle rather than a failure: conn.Peer().AuthMethods
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
func printUpdate(_ context.Context, notification *acp.SessionNotification) {
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

// A workspace is the directory this client is willing to expose, and the user it
// can ask about it.
//
// Containment is enforced here because nothing else can enforce it: an agent may
// name any path it likes, and the capability it was granted says "this client
// reads files", not "this client reads that file".
type workspace struct {
	root  string
	input *bufio.Reader
}

func (w *workspace) resolve(path string) (string, error) {
	// The schema requires an absolute path, so a relative one is a protocol error
	// rather than something to resolve against a guess.
	if !filepath.IsAbs(path) {
		return "", &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: path + " is not an absolute path"}
	}
	clean := filepath.Clean(path)
	if clean != w.root && !strings.HasPrefix(clean, w.root+string(filepath.Separator)) {
		return "", &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: path + " is outside this workspace"}
	}
	return clean, nil
}

func (w *workspace) readTextFile(
	_ context.Context,
	request *acp.ReadTextFileRequest,
) (*acp.ReadTextFileResponse, error) {
	path, err := w.resolve(request.Path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path) //nolint:gosec // resolve has just confirmed the path is inside the workspace.
	if err != nil {
		return nil, &acp.Error{Code: acp.ErrorCodeResourceNotFound, Message: "cannot read " + request.Path}
	}
	return &acp.ReadTextFileResponse{Content: window(string(content), request.Line, request.Limit)}, nil
}

func (w *workspace) writeTextFile(
	_ context.Context,
	request *acp.WriteTextFileRequest,
) (*acp.WriteTextFileResponse, error) {
	path, err := w.resolve(request.Path)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(request.Content), 0o600); err != nil {
		return nil, &acp.Error{Code: acp.ErrorCodeInternalError, Message: "cannot write " + request.Path}
	}
	return &acp.WriteTextFileResponse{}, nil
}

// The user decides. This client has no policy of its own, which is the honest
// default: the protocol defines no outcome a client may assume.
func (w *workspace) requestPermission(
	_ context.Context,
	request *acp.RequestPermissionRequest,
) (*acp.RequestPermissionResponse, error) {
	title, _ := request.ToolCall.Title.Get()
	fmt.Printf("\n\nthe agent wants to: %s\n", title)
	for index, option := range request.Options {
		fmt.Printf("  %d) %s\n", index+1, option.Name)
	}
	fmt.Print("choose: ")

	// Blocking on a human is safe here: a permission request is a request, and this
	// handler runs on a goroutine of its own. A notification handler must never
	// block like this — see the package documentation.
	line, err := w.input.ReadString('\n')
	fmt.Println()
	choice, numberErr := strconv.Atoi(strings.TrimSpace(line))
	if err == nil && numberErr == nil && choice >= 1 && choice <= len(request.Options) {
		return &acp.RequestPermissionResponse{
			Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[choice-1].OptionID},
		}, nil
	}

	// No usable answer. The cancelled outcome is not the way to say no — it means
	// the turn ended — so this falls back to a refusal the agent offered, and fails
	// if it offered none.
	for _, option := range request.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce || option.Kind == acp.PermissionOptionKindRejectAlways {
			fmt.Println("declining")
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: option.OptionID},
			}, nil
		}
	}
	return nil, &acp.Error{
		Code:    acp.ErrorCodeInternalError,
		Message: "nobody answered and no option means no",
	}
}

// The line window the schema defines: a 1-based first line and a maximum count,
// either of which may be absent.
func window(text string, from, limit acp.Opt[uint32]) string {
	lines := strings.SplitAfter(text, "\n")
	if start, given := from.Get(); given && start > 1 {
		if uint64(start-1) >= uint64(len(lines)) {
			return ""
		}
		lines = lines[start-1:]
	}
	if count, given := limit.Get(); given && uint64(count) < uint64(len(lines)) {
		lines = lines[:count]
	}
	return strings.Join(lines, "")
}
