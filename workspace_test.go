package acp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// The workspace operations, driven from an agent's prompt handler, which is the
// only place they happen: the agent does the work and the client owns the files
// and the terminals.

func TestTheFilesystemOperations(t *testing.T) {
	files := map[string]string{"/w/a.go": "package main\n"}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		ReadTextFile: func(
			_ context.Context,
			request *acp.ReadTextFileRequest,
		) (*acp.ReadTextFileResponse, error) {
			content, ok := files[request.Path]
			if !ok {
				return nil, &acp.Error{Code: acp.ErrorCodeResourceNotFound, Message: request.Path}
			}
			return &acp.ReadTextFileResponse{Content: content}, nil
		},
		WriteTextFile: func(
			_ context.Context,
			request *acp.WriteTextFileRequest,
		) (*acp.WriteTextFileResponse, error) {
			files[request.Path] = request.Content
			return &acp.WriteTextFileResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var read string
	var readMissing, wrote error
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		file, err := session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/w/a.go"})
		if err != nil {
			return nil, err
		}
		read = file.Content

		_, readMissing = session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/w/nope.go"})

		_, wrote = session.WriteTextFile(ctx, &acp.WriteTextFileParams{
			Path:    "/w/b.go",
			Content: "package other\n",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if read != "package main\n" {
		t.Errorf("read %q", read)
	}
	if wrote != nil {
		t.Errorf("WriteTextFile: %v", wrote)
	}
	if files["/w/b.go"] != "package other\n" {
		t.Errorf("the file was not written: %q", files["/w/b.go"])
	}

	// A handler that returns an *Error chooses the code, and it survives the round
	// trip so that a caller can act on it.
	if readMissing == nil {
		t.Fatal("reading a missing file succeeded")
	}
	var failure *acp.Error
	if !errors.As(readMissing, &failure) {
		t.Fatalf("the failure is a %T, want *acp.Error", readMissing)
	}
	if failure.Code != acp.ErrorCodeResourceNotFound {
		t.Errorf("code = %s, want resource not found", failure.Code)
	}
}

// The terminal group, all five methods, because the capability that gates them is
// one boolean covering all five.
func TestTheTerminalOperations(t *testing.T) {
	killed := false
	released := false

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(
				_ context.Context,
				request *acp.CreateTerminalRequest,
			) (*acp.CreateTerminalResponse, error) {
				if request.Command != "/bin/go" {
					return nil, errors.New("unexpected command")
				}
				return &acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
			},
			Output: func(
				_ context.Context,
				_ *acp.TerminalOutputRequest,
			) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{Output: "ok\n", Truncated: false}, nil
			},
			WaitForExit: func(
				_ context.Context,
				_ *acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{ExitCode: acp.OptValue(uint32(0))}, nil
			},
			Kill: func(_ context.Context, _ *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				killed = true
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(_ context.Context, _ *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				released = true
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var terminalID acp.TerminalID
	var output string
	var exitCode acp.Opt[uint32]
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		terminal, created, err := session.CreateTerminal(ctx, &acp.CreateTerminalParams{
			Command: "/bin/go",
			Args:    []string{"test", "./..."},
			Env:     []acp.EnvVariable{{Name: "GOFLAGS", Value: "-count=1"}},
		})
		if err != nil {
			return nil, err
		}
		// The handle binds both identifiers, and the response is returned as well
		// because it carries _meta besides the identifier.
		terminalID = terminal.ID()
		if created.TerminalID != terminal.ID() {
			return nil, errors.New("the handle and the response disagree about the identifier")
		}
		if terminal.Session().ID() != session.ID() {
			return nil, errors.New("the handle points at another session")
		}

		out, err := terminal.Output(ctx, nil)
		if err != nil {
			return nil, err
		}
		output = out.Output

		exited, err := terminal.WaitForExit(ctx, nil)
		if err != nil {
			return nil, err
		}
		exitCode = exited.ExitCode

		if _, err := terminal.Kill(ctx, nil); err != nil {
			return nil, err
		}
		if _, err := terminal.Release(ctx, nil); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if terminalID != "term-1" {
		t.Errorf("terminal id = %q", terminalID)
	}
	if output != "ok\n" {
		t.Errorf("output = %q", output)
	}
	if code, ok := exitCode.Get(); !ok || code != 0 {
		t.Errorf("exit code = %v, %t", code, ok)
	}
	if !killed || !released {
		t.Errorf("killed = %t, released = %t", killed, released)
	}
}

// The terminal handlers are all five or none, because the capability is one
// boolean. A partial set is a client that would refuse a method it advertised.
func TestAPartialTerminalHandlerSetIsRefused(t *testing.T) {
	_, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				return &acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
			},
		},
	})
	if err == nil {
		t.Fatal("a client with one of five terminal handlers was accepted")
	}
	for _, missing := range []string{"Kill", "Output", "Release", "WaitForExit"} {
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the failure does not name the missing %s handler: %v", missing, err)
		}
	}
}

// An advertisement this client cannot serve is refused at construction, before it
// can accept a request it would have to fail.
func TestAnAdvertisementBeyondTheHandlersIsRefused(t *testing.T) {
	_, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Capabilities: &acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true},
			Terminal: true,
		},
	})
	if err == nil {
		t.Fatal("a client advertising what it cannot serve was accepted")
	}
	if !strings.Contains(err.Error(), "fs.readTextFile") || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("the failure does not name both claims: %v", err)
	}
}

// The baseline handlers are not optional, on either side.
func TestTheBaselineHandlersAreRequired(t *testing.T) {
	if _, err := acp.NewClient(&acp.ClientConfig{
		RequestPermission: denyingPermission,
	}); err == nil {
		t.Error("a client with no SessionUpdate handler was accepted")
	}
	if _, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
	}); err == nil {
		t.Error("a client with no RequestPermission handler was accepted: there is no outcome to synthesise")
	}
	if _, err := acp.NewClient(nil); err == nil {
		t.Error("a nil client configuration was accepted")
	}

	_, err := acp.NewAgent(&acp.AgentConfig{})
	if err == nil {
		t.Fatal("an agent with no handlers at all was accepted")
	}
	for _, missing := range []string{"NewSession", "Prompt", "Cancel"} {
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the failure does not name the missing %s handler: %v", missing, err)
		}
	}
	if _, err := acp.NewAgent(nil); err == nil {
		t.Error("a nil agent configuration was accepted")
	}
}

// A released terminal handle is spent, and says so rather than sending.
//
// The contract was documented and not kept: Output, WaitForExit, Kill and a
// second Release all went on sending. A released identifier is the client's to
// reuse, so those requests are not merely pointless — they may name a terminal
// that now belongs to something else, and the client would serve them.
func TestAReleasedTerminalHandleRefusesEverything(t *testing.T) {
	served := make(chan string, 8)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				served <- "create"
				return &acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				served <- "output"
				return &acp.TerminalOutputResponse{}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				served <- "wait"
				return &acp.WaitForTerminalExitResponse{}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				served <- "kill"
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(context.Context, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				served <- "release"
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	refusals := make(chan error, 4)
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		terminal, _, err := session.CreateTerminal(ctx, &acp.CreateTerminalParams{Command: "/bin/true"})
		if err != nil {
			return nil, err
		}
		if _, released := terminal.Release(ctx, nil); released != nil {
			return nil, released
		}

		_, err = terminal.Output(ctx, nil)
		refusals <- err
		_, err = terminal.WaitForExit(ctx, nil)
		refusals <- err
		_, err = terminal.Kill(ctx, nil)
		refusals <- err
		_, err = terminal.Release(ctx, nil)
		refusals <- err
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	close(refusals)
	for err := range refusals {
		if !errors.Is(err, acp.ErrTerminalReleased) {
			t.Errorf("an operation on a released handle returned %v, want ErrTerminalReleased", err)
		}
	}

	close(served)
	var reached []string
	for method := range served {
		reached = append(reached, method)
	}
	if len(reached) != 2 || reached[0] != "create" || reached[1] != "release" {
		t.Fatalf("the client served %v; a released handle went on sending", reached)
	}
}

func TestATerminalReleaseCanRetryWhenNothingWasSent(t *testing.T) {
	released := make(chan struct{}, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				return &acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(context.Context, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				released <- struct{}{}
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	retried := make(chan error, 1)
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		terminal, _, err := session.CreateTerminal(ctx, &acp.CreateTerminalParams{Command: "/bin/true"})
		if err != nil {
			return nil, err
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, cancelErr := terminal.Release(cancelled, nil); !errors.Is(cancelErr, context.Canceled) {
			return nil, fmt.Errorf("release with cancelled context returned %w", cancelErr)
		}
		_, err = terminal.Release(ctx, nil)
		retried <- err
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := <-retried; err != nil {
		t.Fatalf("Release retry: %v", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("the retried release never reached the client")
	}
}

// An empty session or terminal identifier is a string, and the schema says no
// more than that.
//
// The pinned definitions of SessionId and TerminalId declare no minLength, so the
// reference implementation accepts an empty one. Refusing it here was this
// package writing a constraint into a grammar it does not own, and the repository
// rule is that the schema wins.
func TestAnEmptyIdentifierIsAcceptedBecauseTheSchemaDeclaresNoMinimum(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				return &acp.CreateTerminalResponse{TerminalID: ""}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(context.Context, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	created := make(chan error, 1)
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: ""}, nil
		},
		Prompt: func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			_, _, failure := session.CreateTerminal(ctx, &acp.CreateTerminalParams{Command: "/bin/true"})
			created <- failure
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, client, agent)
	if session.ID() != "" {
		t.Fatalf("the session identifier came back as %q", session.ID())
	}
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := <-created; err != nil {
		t.Fatalf("creating a terminal with an empty identifier failed: %v", err)
	}
}

// "All file paths in the protocol MUST be absolute." A relative one is refused
// before it is sent, in both directions: an agent's workspace call and a client's
// session setup.
//
// The check accepts either convention rather than the running process's, because
// the path describes the peer's filesystem and nothing requires the two peers to
// share an operating system.
func TestARelativePathIsRefusedBeforeItIsSent(t *testing.T) {
	var reached int
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		ReadTextFile: func(context.Context, *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
			reached++
			return &acp.ReadTextFileResponse{}, nil
		},
		WriteTextFile: func(context.Context, *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
			reached++
			return &acp.WriteTextFileResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var relative, posix, windows error
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, relative = session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "notes/a.go"})
		_, posix = session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/w/a.go"})
		_, windows = session.WriteTextFile(ctx, &acp.WriteTextFileParams{
			Path: `C:\w\a.go`, Content: "x",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if relative == nil {
		t.Fatal("a relative path reached the client")
	}
	if !strings.Contains(relative.Error(), "absolute") {
		t.Errorf("the refusal does not say what is wrong: %v", relative)
	}
	if posix != nil || windows != nil {
		t.Errorf("an absolute path was refused: posix=%v windows=%v", posix, windows)
	}
	if reached != 2 {
		t.Errorf("the client served %d calls, want the 2 absolute ones", reached)
	}

	// And the same rule on the session the client opens.
	conn := session.Conn()
	if _, _, err := conn.NewSession(context.Background(), &acp.NewSessionRequest{Cwd: "w"}); err == nil ||
		!strings.Contains(err.Error(), "cwd must be an absolute path") {
		t.Errorf("a relative cwd was not refused: %v", err)
	}
}
