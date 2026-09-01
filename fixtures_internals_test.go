package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// errUnexpectedArm reports a fixture whose registered codec does not match the
// arm the decoder produced, which would mean the registry and the union
// disagree about what the union's arms are.
var errUnexpectedArm = errors.New("the decoded value is not the arm type this codec encodes")

// The fixture corpus is the layer-1 cross-check: the TypeScript SDK decides what
// each input means, and this replays those answers against the Go codec. Two
// endpoints built from one Go implementation can agree with each other and both
// be wrong, so the oracle has to be the reference implementation.
//
// It is committed rather than computed, so this runs with no network and no Node
// toolchain. scripts/update-fixtures.sh is what produces it, from a pinned SDK
// commit and the pinned schema release.
//
// The promise checked here is semantic, not byte identity: both SDKs accept the
// same input and normalise it to equivalent JSON. Whitespace, key order, number
// spelling and escapes may all differ while the decoded value is the same — and
// they do differ, because the reference implementation emits its properties in
// validator order while this package emits them in schema order.
type fixtureFile struct {
	Type  string         `json:"type"`
	Cases []*fixtureCase `json:"cases"`
}

type fixtureCase struct {
	Name string `json:"name"`
	// Type overrides the file's, for a file that groups several definitions.
	Type string `json:"type"`
	// Why is the human half of the corpus: what wire behaviour the case is here
	// to pin down. The oracle preserves it and never writes it.
	Why   string          `json:"why"`
	Input json.RawMessage `json:"input"`
	// Accepted is a pointer so that a fixture the oracle has never seen is a test
	// failure rather than a case that quietly expects rejection.
	Accepted   *bool           `json:"accepted"`
	Normalized json.RawMessage `json:"normalized"`
	// Divergence records a case where this package deliberately disagrees with the
	// reference implementation. The oracle never writes it.
	Divergence *fixtureDivergence `json:"divergence"`
}

// A fixtureDivergence is a place where the reference implementation and the
// published schema disagree, and the schema wins.
//
// AGENTS.md is unambiguous about this: the wire grammar is not this repository's
// to design, and where it disagrees with the published schema the schema is right.
// The reference implementation is the oracle for behaviour the schema leaves
// unstated — which fallback a recovered property takes, which arm an ambiguous
// value lands in — and not an authority over what the schema does state.
//
// So a divergence is not an escape hatch for a failing test. It records the
// schema clause that decides the case, and every one of them is worth reporting
// upstream. Recording it here rather than deleting the case is what keeps the
// disagreement visible.
type fixtureDivergence struct {
	Accepted   *bool           `json:"accepted"`
	Normalized json.RawMessage `json:"normalized"`
	Because    string          `json:"because"`
}

// A fixtureCodec is how one definition is decoded and re-encoded. Unions need
// their own entry because a Go interface cannot decode into itself: the arm is
// selected by a generated function.
type fixtureCodec struct {
	decode func(json.RawMessage) (any, error)
	encode func(any) ([]byte, error)
}

func valueCodec[T any]() fixtureCodec {
	return fixtureCodec{
		decode: func(raw json.RawMessage) (any, error) {
			value := new(T)
			if err := json.Unmarshal(raw, value); err != nil {
				return nil, err
			}
			return value, nil
		},
		encode: json.Marshal,
	}
}

func unionCodec[T any](
	decode func(json.RawMessage) (T, error),
	encode func(T) ([]byte, error),
) fixtureCodec {
	return fixtureCodec{
		decode: func(raw json.RawMessage) (any, error) {
			value, err := decode(raw)
			if err != nil {
				return nil, err
			}
			return value, nil
		},
		encode: func(value any) ([]byte, error) {
			arm, ok := value.(T)
			if !ok {
				return nil, errUnexpectedArm
			}
			return encode(arm)
		},
	}
}

var fixtureCodecs = map[string]fixtureCodec{
	"CancelNotification": valueCodec[CancelNotification](),
	// The schema's name, not the Go one: this payload is generated unexported,
	// because a caller cancels by cancelling a context and never names a request
	// identifier. The oracle is asked about the schema's name too.
	"CancelRequestNotification":   valueCodec[cancelRequestNotification](),
	"CreateTerminalRequest":       valueCodec[CreateTerminalRequest](),
	"Error":                       valueCodec[Error](),
	"FileSystemCapabilities":      valueCodec[FileSystemCapabilities](),
	"InitializeRequest":           valueCodec[InitializeRequest](),
	"InitializeResponse":          valueCodec[InitializeResponse](),
	"MultiSelectItems":            unionCodec(unmarshalMultiSelectItems, marshalMultiSelectItems),
	"NewSessionRequest":           valueCodec[NewSessionRequest](),
	"NewSessionResponse":          valueCodec[NewSessionResponse](),
	"PromptRequest":               valueCodec[PromptRequest](),
	"PromptResponse":              valueCodec[PromptResponse](),
	"ProtocolVersion":             valueCodec[ProtocolVersion](),
	"ReadTextFileRequest":         valueCodec[ReadTextFileRequest](),
	"ReadTextFileResponse":        valueCodec[ReadTextFileResponse](),
	"RequestPermissionRequest":    valueCodec[RequestPermissionRequest](),
	"RequestPermissionResponse":   valueCodec[RequestPermissionResponse](),
	"SessionConfigOptionCategory": valueCodec[SessionConfigOptionCategory](),
	"SessionID":                   valueCodec[SessionID](),
	"SessionNotification":         valueCodec[SessionNotification](),
	"SetSessionModeRequest":       valueCodec[SetSessionModeRequest](),
	"StopReason":                  valueCodec[StopReason](),
	"TerminalOutputResponse":      valueCodec[TerminalOutputResponse](),
	"WaitForTerminalExitResponse": valueCodec[WaitForTerminalExitResponse](),
	"WriteTextFileRequest":        valueCodec[WriteTextFileRequest](),
	"WriteTextFileResponse":       valueCodec[WriteTextFileResponse](),
}

func TestFixturesAgreeWithTheReferenceImplementation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "fixtures", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures found; the corpus is the layer-1 cross-check and cannot be empty")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			// The path came from globbing this repository's own testdata, so the
			// file-inclusion warning has nothing to warn about.
			raw, err := os.ReadFile(path) //nolint:gosec // the path is a glob result under testdata.
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var fixture fixtureFile
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			for _, testCase := range fixture.Cases {
				runFixtureCase(t, fixture, testCase)
			}
		})
	}
}

func runFixtureCase(t *testing.T, fixture fixtureFile, testCase *fixtureCase) {
	t.Helper()
	t.Run(testCase.Name, func(t *testing.T) {
		typeName := testCase.Type
		if typeName == "" {
			typeName = fixture.Type
		}
		codec, ok := fixtureCodecs[typeName]
		if !ok {
			t.Fatalf("no codec for %s; a fixture the test cannot run is not evidence", typeName)
		}
		if testCase.Accepted == nil {
			t.Fatalf("no recorded outcome; run scripts/update-fixtures.sh")
		}
		if testCase.Why == "" {
			t.Fatalf("no stated reason; a case that does not say what it pins down cannot be reviewed")
		}

		accepted, normalized, authority := *testCase.Accepted, testCase.Normalized, "the reference implementation"
		if testCase.Divergence != nil {
			if err := checkDivergence(testCase); err != nil {
				t.Fatal(err)
			}
			accepted, normalized = *testCase.Divergence.Accepted, testCase.Divergence.Normalized
			authority = "the schema, which this case says the reference implementation disagrees with"
		}

		value, err := codec.decode(testCase.Input)
		if !accepted {
			if err == nil {
				t.Fatalf("decoded %s, which %s refuses (%s)", testCase.Input, authority, testCase.Why)
			}
			return
		}
		if err != nil {
			t.Fatalf("decoding %s failed with %v, but %s accepts it (%s)",
				testCase.Input, err, authority, testCase.Why)
		}

		encoded, err := codec.encode(value)
		if err != nil {
			t.Fatalf("re-encoding failed: %v", err)
		}
		if !equivalentJSON(t, encoded, normalized) {
			t.Fatalf("normalised to %s, want the equivalent of %s (%s)", encoded, normalized, testCase.Why)
		}
	})
}

// A divergence has to be a real one, and it has to say which clause of the schema
// decides it. Otherwise the mechanism would be a way to make any failing case pass
// by asserting that this package is right.
func checkDivergence(testCase *fixtureCase) error {
	divergence := testCase.Divergence
	switch {
	case divergence.Accepted == nil:
		return fmt.Errorf("case %q records a divergence without saying what this package does instead",
			testCase.Name)
	case divergence.Because == "":
		return fmt.Errorf("case %q records a divergence without naming the schema clause that decides it",
			testCase.Name)
	case *divergence.Accepted != *testCase.Accepted:
		return nil
	case *divergence.Accepted && string(divergence.Normalized) == "":
		return fmt.Errorf("case %q diverges on the normalised value without giving one", testCase.Name)
	case !*divergence.Accepted:
		return fmt.Errorf("case %q records a divergence that agrees with the reference implementation",
			testCase.Name)
	default:
		return nil
	}
}

// equivalentJSON compares two encodings by what they decode to. Neither SDK
// promises byte identity and neither could: the reference implementation parses
// _meta into ordinary values and re-serialises them, so its original bytes are
// gone too.
func equivalentJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var decodedGot, decodedWant any
	if err := json.Unmarshal(got, &decodedGot); err != nil {
		t.Fatalf("the Go encoding is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(want, &decodedWant); err != nil {
		t.Fatalf("the recorded expectation is not valid JSON: %v", err)
	}
	return reflect.DeepEqual(decodedGot, decodedWant)
}
