package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tangerg/acp"
)

func TestWorkspaceKeepsSymlinksInsideItsRoot(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	place, err := openWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = place.root.Close() }()

	for _, test := range []struct {
		name   string
		target string
		file   string
	}{
		{name: "file-link", target: target},
		{name: "directory-link", target: outside, file: "secret.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			link := filepath.Join(root, test.name)
			if err := os.Symlink(test.target, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlinks unavailable in this Windows environment: %v", err)
				}
				t.Fatal(err)
			}
			path := filepath.Join(link, test.file)
			if _, err := place.readTextFile(t.Context(), nil, &acp.ReadTextFileRequest{Path: path}); err == nil {
				t.Fatal("read followed a link outside the workspace")
			}
			if _, err := place.writeTextFile(t.Context(), nil, &acp.WriteTextFileRequest{Path: path, Content: "overwrite"}); err == nil {
				t.Fatal("write followed a link outside the workspace")
			}
			content, err := os.ReadFile(target) //nolint:gosec // The path names only this test's temporary fixture.
			if err != nil || string(content) != "private" {
				t.Fatalf("outside file = %q, %v", content, err)
			}
		})
	}
}

func TestWorkspaceReadsAndWritesOnlyLocalPaths(t *testing.T) {
	root := t.TempDir()
	place, err := openWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = place.root.Close() }()
	path := filepath.Join(root, "notes.txt")
	if _, err := place.writeTextFile(t.Context(), nil, &acp.WriteTextFileRequest{Path: path, Content: "one\ntwo\n"}); err != nil {
		t.Fatal(err)
	}
	read, err := place.readTextFile(t.Context(), nil, &acp.ReadTextFileRequest{Path: path, Line: acp.OptValue(uint32(2))})
	if err != nil || read.Content != "two\n" {
		t.Fatalf("read = %#v, %v", read, err)
	}
	for _, path := range []string{"notes.txt", filepath.Join(t.TempDir(), "notes.txt")} {
		if _, err := place.readTextFile(t.Context(), nil, &acp.ReadTextFileRequest{Path: path}); err == nil {
			t.Fatalf("accepted path outside the workspace: %q", path)
		}
	}
}

func TestWorkspaceDoesNotCallEveryReadErrorMissing(t *testing.T) {
	root := t.TempDir()
	place, err := openWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = place.root.Close() }()
	_, err = place.readTextFile(t.Context(), nil, &acp.ReadTextFileRequest{Path: filepath.Join(root, "missing")})
	var failure *acp.Error
	if !errors.As(err, &failure) || failure.Code != acp.ErrorCodeResourceNotFound {
		t.Fatalf("missing file = %v", err)
	}
	_, err = place.readTextFile(t.Context(), nil, &acp.ReadTextFileRequest{Path: root})
	if err == nil || errors.As(err, &failure) && failure.Code == acp.ErrorCodeResourceNotFound {
		t.Fatalf("directory read falsely classified as absence: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = place.writeTextFile(ctx, nil, &acp.WriteTextFileRequest{Path: filepath.Join(root, "cancelled"), Content: "no"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write = %v", err)
	}
}
