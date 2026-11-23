package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Tangerg/acp"
)

const (
	notesFilename                           = "NOTES.md"
	permissionAppend acp.PermissionOptionID = "allow"
	permissionReject acp.PermissionOptionID = "reject"
)

type sessions struct {
	mu    sync.Mutex
	count uint64
	open  map[acp.SessionID]*noteSession
	files map[noteFileKey]*noteFile
}

type noteFileKey struct {
	connection *acp.AgentConn
	path       string
}

type noteSession struct {
	notes     *noteFile
	toolCalls atomic.Uint64
}

// A file can belong to several conversations. Its owner serializes the complete
// read-modify-write; a lock on session lookup cannot protect that transaction.
// ACP read/write has no CAS, so writes by another process remain outside it.
type noteFile struct {
	path    string
	writing chan struct{}
}

func newSessions() *sessions {
	return &sessions{
		open:  make(map[acp.SessionID]*noteSession),
		files: make(map[noteFileKey]*noteFile),
	}
}

func (s *sessions) newSession(
	_ context.Context,
	conn *acp.AgentConn,
	request *acp.NewSessionRequest,
) (*acp.NewSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := noteFileKey{connection: conn, path: filepath.Join(request.Cwd, notesFilename)}
	notes := s.files[key]
	if notes == nil {
		notes = &noteFile{path: key.path, writing: make(chan struct{}, 1)}
		s.files[key] = notes
	}
	s.count++
	id := acp.SessionID(fmt.Sprintf("session-%d", s.count))
	s.open[id] = &noteSession{notes: notes}
	return &acp.NewSessionResponse{SessionID: id}, nil
}

func (s *sessions) prompt(
	ctx context.Context,
	session *acp.AgentSession,
	request *acp.PromptRequest,
) (*acp.PromptResponse, error) {
	s.mu.Lock()
	conversation := s.open[session.ID()]
	s.mu.Unlock()
	if conversation == nil {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "no such session"}
	}
	return conversation.prompt(ctx, session, request)
}

func (s *noteSession) prompt(
	ctx context.Context,
	session *acp.AgentSession,
	request *acp.PromptRequest,
) (*acp.PromptResponse, error) {
	capabilities := session.Conn().Peer().ClientCapabilities.Fs
	if !capabilities.ReadTextFile || !capabilities.WriteTextFile {
		if err := say(ctx, session, "Appending a note requires both file read and write access."); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if err := say(ctx, session, "I will append that to "+s.notes.path+"."); err != nil {
		return nil, err
	}
	toolID := acp.ToolCallID(fmt.Sprintf("write-%d", s.toolCalls.Add(1)))
	decision, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{
			ToolCallID: toolID,
			Title:      acp.OptValue("append a note to " + s.notes.path),
			Kind:       acp.OptValue(acp.ToolKindEdit),
		},
		Options: []acp.PermissionOption{
			{OptionID: permissionAppend, Name: "Append it", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: permissionReject, Name: "Leave it alone", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return nil, err
	}
	selected, chosen := decision.Outcome.(*acp.SelectedPermissionOutcome)
	if !chosen {
		return &acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if selected.OptionID != permissionAppend {
		if sayErr := say(ctx, session, "Left it alone."); sayErr != nil {
			return nil, sayErr
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if appendErr := s.notes.append(ctx, session, promptText(request)); appendErr != nil {
		return nil, appendErr
	}
	if sayErr := say(ctx, session, "Done."); sayErr != nil {
		return nil, sayErr
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *noteFile) append(ctx context.Context, session *acp.AgentSession, note string) error {
	select {
	case f.writing <- struct{}{}:
		defer func() { <-f.writing }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var existing string
	read, err := session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: f.path})
	var failure *acp.Error
	switch {
	case err == nil:
		existing = read.Content
	case errors.As(err, &failure) && failure.Code == acp.ErrorCodeResourceNotFound:
		// Only confirmed absence permits starting a file. An unreadable file
		// may contain notes this operation has no authority to discard.
	default:
		return err
	}
	_, writeErr := session.WriteTextFile(ctx, &acp.WriteTextFileParams{
		Path:    f.path,
		Content: existing + "- " + note + "\n",
	})
	return writeErr
}
