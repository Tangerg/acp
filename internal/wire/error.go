package wire

import (
	"errors"
	"strconv"
	"strings"
)

// ErrRequired reports a required property that the message did not carry.
//
// It is a distinct error rather than a message because recovery depends on it:
// a property marked x-deserialize-default-on-error recovers from a malformed
// value but must still fail when it is absent, and the two cases are only
// distinguishable if the absent one is identifiable.
var ErrRequired = errors.New("required property is missing")

// ErrNotNullable reports a property present as null that the schema does not
// permit to be null.
//
// It has to be checked explicitly. Unmarshalling JSON null into a Go value
// leaves the value alone and reports no error, so without this a null would
// arrive as the zero value and a message the schema rejects would decode
// cleanly.
var ErrNotNullable = errors.New("property may not be null")

// A PathError says where in a JSON value a decode or validation failure was, as
// a JSON pointer.
//
// Without it a nested failure reports only what went wrong, and the caller of a
// protocol method that carries a fifteen-arm union inside an array inside a
// request has no way to find the offending property.
type PathError struct {
	// Path is a JSON pointer relative to the value being decoded, or empty when
	// the failure is the value itself.
	Path string
	Err  error
}

func (e *PathError) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e *PathError) Unwrap() error { return e.Err }

// At reports err as having happened at property name, prepending name to the
// path err already carries. The outermost caller therefore names the property
// closest to the root, and the accumulated path reads from the root down.
func At(name string, err error) error {
	return prepend("/"+escapePointer(name), err)
}

// Index reports err as having happened at array element i.
func Index(i int, err error) error {
	return prepend("/"+strconv.Itoa(i), err)
}

// prepend flattens rather than nests, so one failure reports one pointer
// instead of a chain of them. errors.As is exact for this because generated code
// either returns a PathError or hands it straight to [At] or [Index]; it never
// wraps one inside another message, which is the only case where the innermost
// match would not be the outermost.
func prepend(segment string, err error) error {
	var pe *PathError
	if errors.As(err, &pe) {
		return &PathError{Path: segment + pe.Path, Err: pe.Err}
	}
	return &PathError{Path: segment, Err: err}
}

// escapePointer applies RFC 6901 escaping. No property name in the published
// schema needs it, which is exactly why it is done here rather than trusted to
// stay that way.
func escapePointer(name string) string {
	if !strings.ContainsAny(name, "~/") {
		return name
	}
	name = strings.ReplaceAll(name, "~", "~0")
	return strings.ReplaceAll(name, "/", "~1")
}
