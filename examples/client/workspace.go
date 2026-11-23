package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tangerg/acp"
)

const workspaceFileMode = 0o600

// The open directory owns containment throughout I/O, including symlink
// resolution. A path check followed by ordinary file I/O cannot make that promise.
type workspace struct {
	root *os.Root
}

func openWorkspace(path string) (*workspace, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &workspace{root: root}, nil
}

func (w *workspace) relativePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: path + " is not an absolute path"}
	}
	relative, err := filepath.Rel(w.root.Name(), path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: path + " is outside this workspace"}
	}
	return relative, nil
}

func (w *workspace) readTextFile(
	ctx context.Context,
	_ *acp.ClientConn,
	request *acp.ReadTextFileRequest,
) (*acp.ReadTextFileResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := w.relativePath(request.Path)
	if err != nil {
		return nil, err
	}
	content, readErr := w.root.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, &acp.Error{Code: acp.ErrorCodeResourceNotFound, Message: "no such file: " + request.Path}
	}
	if readErr != nil {
		return nil, fmt.Errorf("read workspace file: %w", readErr)
	}
	return &acp.ReadTextFileResponse{Content: window(string(content), request.Line, request.Limit)}, nil
}

func (w *workspace) writeTextFile(
	ctx context.Context,
	_ *acp.ClientConn,
	request *acp.WriteTextFileRequest,
) (*acp.WriteTextFileResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := w.relativePath(request.Path)
	if err != nil {
		return nil, err
	}
	if writeErr := w.root.WriteFile(path, []byte(request.Content), workspaceFileMode); writeErr != nil {
		return nil, fmt.Errorf("write workspace file: %w", writeErr)
	}
	return &acp.WriteTextFileResponse{}, nil
}

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
