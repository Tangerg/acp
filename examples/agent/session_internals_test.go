package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp"
)

func connectExampleAgent(t *testing.T, config *acp.ClientConfig) (*acp.ClientConn, func()) {
	t.Helper()
	if config.SessionUpdate == nil {
		config.SessionUpdate = func(context.Context, *acp.ClientConn, *acp.SessionNotification) {}
	}
	if config.RequestPermission == nil {
		config.RequestPermission = func(context.Context, *acp.ClientConn, *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
			return &acp.RequestPermissionResponse{Outcome: &acp.SelectedPermissionOutcome{OptionID: "allow"}}, nil
		}
	}
	client, err := acp.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	conversations := newSessions()
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: conversations.newSession,
		Prompt:     conversations.prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientSide, agentSide := acp.NewInMemoryTransports()
	agentConn, err := agent.Connect(t.Context(), agentSide)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := client.Connect(t.Context(), clientSide)
	if err != nil {
		_ = agentConn.Close()
		t.Fatal(err)
	}
	return conn, func() {
		_ = conn.Close()
		_ = agentConn.Close()
	}
}

func newExampleSession(t *testing.T, conn *acp.ClientConn, cwd string) *acp.ClientSession {
	t.Helper()
	session, _, err := conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestNotesNeverWriteUnreadContents(t *testing.T) {
	for _, name := range []string{"read-error", "no-read-capability"} {
		t.Run(name, func(t *testing.T) {
			var writes []string
			config := &acp.ClientConfig{
				WriteTextFile: func(_ context.Context, _ *acp.ClientConn, request *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
					writes = append(writes, request.Content)
					return &acp.WriteTextFileResponse{}, nil
				},
			}
			if name == "read-error" {
				config.ReadTextFile = func(context.Context, *acp.ClientConn, *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
					return nil, errors.New("cannot read the existing notes")
				}
			}
			conn, closeConnection := connectExampleAgent(t, config)
			defer closeConnection()
			session := newExampleSession(t, conn, t.TempDir())
			_, err := session.Prompt(t.Context(), &acp.PromptParams{Prompt: []acp.ContentBlock{&acp.TextContent{Text: "new note"}}})
			if name == "read-error" && err == nil {
				t.Fatal("reading failed but the prompt reported success")
			}
			if len(writes) != 0 {
				t.Fatalf("overwrote unread notes with %q", writes)
			}
		})
	}
}

func TestNotesCreateOnlyOnConfirmedAbsence(t *testing.T) {
	var content string
	conn, closeConnection := connectExampleAgent(t, &acp.ClientConfig{
		ReadTextFile: func(context.Context, *acp.ClientConn, *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
			return nil, &acp.Error{Code: acp.ErrorCodeResourceNotFound, Message: "no such file"}
		},
		WriteTextFile: func(_ context.Context, _ *acp.ClientConn, request *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
			content = request.Content
			return &acp.WriteTextFileResponse{}, nil
		},
	})
	defer closeConnection()
	session := newExampleSession(t, conn, t.TempDir())
	if _, err := session.Prompt(t.Context(), &acp.PromptParams{Prompt: []acp.ContentBlock{&acp.TextContent{Text: "first note"}}}); err != nil {
		t.Fatal(err)
	}
	if content != "- first note\n" {
		t.Fatalf("new file = %q", content)
	}
}

func TestConcurrentConversationsPreserveBothNotes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		content := "- original\n"
		reads := 0
		readAllowed := make(chan struct{})
		conn, closeConnection := connectExampleAgent(t, &acp.ClientConfig{
			ReadTextFile: func(ctx context.Context, _ *acp.ClientConn, _ *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
				mu.Lock()
				reads++
				snapshot := content
				mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-readAllowed:
					return &acp.ReadTextFileResponse{Content: snapshot}, nil
				}
			},
			WriteTextFile: func(_ context.Context, _ *acp.ClientConn, request *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
				mu.Lock()
				content = request.Content
				mu.Unlock()
				return &acp.WriteTextFileResponse{}, nil
			},
		})
		defer closeConnection()
		cwd := t.TempDir()
		first := newExampleSession(t, conn, cwd)
		second := newExampleSession(t, conn, cwd)
		results := make(chan error, 2)
		for index, session := range []*acp.ClientSession{first, second} {
			go func() {
				text := []string{"note-a", "note-b"}[index]
				_, err := session.Prompt(t.Context(), &acp.PromptParams{Prompt: []acp.ContentBlock{&acp.TextContent{Text: text}}})
				results <- err
			}()
		}
		synctest.Wait()
		mu.Lock()
		observed := reads
		mu.Unlock()
		if observed != 1 {
			t.Fatalf("%d transactions read the same uncommitted file", observed)
		}
		close(readAllowed)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		final := content
		mu.Unlock()
		for _, line := range []string{"- original\n", "- note-a\n", "- note-b\n"} {
			if strings.Count(final, line) != 1 {
				t.Fatalf("missing or duplicated note %q in %q", line, final)
			}
		}
	})
}

func TestToolCallIDsAreUniqueAcrossTurns(t *testing.T) {
	var ids []acp.ToolCallID
	conn, closeConnection := connectExampleAgent(t, &acp.ClientConfig{
		RequestPermission: func(_ context.Context, _ *acp.ClientConn, request *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
			ids = append(ids, request.ToolCall.ToolCallID)
			return &acp.RequestPermissionResponse{Outcome: &acp.SelectedPermissionOutcome{OptionID: "reject"}}, nil
		},
		ReadTextFile: func(context.Context, *acp.ClientConn, *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
			t.Error("read after rejecting permission")
			return &acp.ReadTextFileResponse{}, nil
		},
		WriteTextFile: func(context.Context, *acp.ClientConn, *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
			t.Error("write after rejecting permission")
			return &acp.WriteTextFileResponse{}, nil
		},
	})
	defer closeConnection()
	session := newExampleSession(t, conn, t.TempDir())
	for range 2 {
		if _, err := session.Prompt(t.Context(), &acp.PromptParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("different tools reused their session's identity: %v", ids)
	}
}

func TestPromptTextPreservesResourceLinks(t *testing.T) {
	request := &acp.PromptRequest{Prompt: []acp.ContentBlock{
		&acp.TextContent{Text: "Refer to"},
		&acp.ResourceLink{Name: "design", URI: "file:///work/design.md"},
		&acp.TextContent{Text: "before editing."},
	}}
	if got := promptText(request); got != "Refer to design (file:///work/design.md) before editing." {
		t.Fatalf("resource link lost its meaning: %q", got)
	}
}
