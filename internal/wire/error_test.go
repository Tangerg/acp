package wire_test

import (
	"errors"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// The path is built as a failure travels outward, so the outermost caller names
// the property closest to the root and the accumulated pointer reads from the
// root down. A chain of nested errors would report the same fact several times.
func TestPathsAccumulateFromTheRootDown(t *testing.T) {
	leaf := errors.New("not a string")
	err := wire.At("prompt", wire.Index(2, wire.At("text", leaf)))

	var pathErr *wire.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %v is not a PathError", err)
	}
	if pathErr.Path != "/prompt/2/text" {
		t.Fatalf("path = %q, want /prompt/2/text", pathErr.Path)
	}
	if !errors.Is(err, leaf) {
		t.Error("the original failure is not reachable through the path")
	}
	if got, want := err.Error(), "/prompt/2/text: not a string"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// A property name containing a pointer metacharacter has to be escaped, or the
// path it produces means a different location than the one that failed. No name
// in the published schema needs it, which is exactly why it is done here rather
// than trusted to stay that way.
func TestPathsEscapePointerMetacharacters(t *testing.T) {
	err := wire.At("vendor/name~1", errors.New("boom"))

	var pathErr *wire.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %v is not a PathError", err)
	}
	if pathErr.Path != "/vendor~1name~01" {
		t.Fatalf("path = %q, want /vendor~1name~01", pathErr.Path)
	}
}

// A failure at the value itself has no path, and reporting an empty pointer as a
// prefix would be noise.
func TestAPathlessErrorReportsOnlyItsMessage(t *testing.T) {
	pathErr := &wire.PathError{Err: errors.New("expected a JSON object, got null")}
	if got, want := pathErr.Error(), "expected a JSON object, got null"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// The two sentinels are matched rather than read, because recovery branches on
// them: a property marked x-deserialize-default-on-error recovers from a
// malformed value but must still fail when it is absent.
func TestSentinelsAreMatchableThroughAPath(t *testing.T) {
	if !errors.Is(wire.At("cwd", wire.ErrRequired), wire.ErrRequired) {
		t.Error("ErrRequired is not matchable through a path")
	}
	if !errors.Is(wire.At("cwd", wire.ErrNotNullable), wire.ErrNotNullable) {
		t.Error("ErrNotNullable is not matchable through a path")
	}
	if errors.Is(wire.At("cwd", wire.ErrRequired), wire.ErrNotNullable) {
		t.Error("the two sentinels are not distinguishable")
	}
}
