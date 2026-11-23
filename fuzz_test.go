package acp_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// Normalisation has to be a fixed point: whatever a peer sends, decoding it and
// encoding it again must produce something that decodes to the same value.
//
// This is the property schema-directed recovery can quietly break. A fallback
// that produced a value the encoder then wrote differently — or that the decoder
// then recovered from a second time — would make a relayed message drift on every
// hop, and no case table finds that, because the input that triggers it is a
// malformed value somebody has to think of.
//
// A case table also cannot state the other half: that no input at all makes the
// codec panic. Every fallback path is reached by an input that failed, so the
// error paths here are the ordinary ones.
func FuzzPromptRequestNormalisationIsAFixedPoint(f *testing.F) {
	seeds := []string{
		`{"sessionId":"s","prompt":[]}`,
		`{"sessionId":"s","prompt":[{"type":"text","text":"a"}]}`,
		`{"sessionId":"s","prompt":[{"type":"resource","resource":{"uri":"u","blob":"aGk="}}],"_meta":{"k":[1,2]}}`,
		`{"sessionId":"s","prompt":[{"type":"text","text":"a","annotations":{"audience":["user","x"]}}]}`,
		`{"sessionId":"s","prompt":[{"type":"text","text":"a"}],"_meta":null}`,
		`{"sessionId":"s","prompt":[{"type":"text","text":"a"}],"_meta":7}`,
		`{"sessionId":"s","prompt":[{"type":"text"}]}`,
		`{"sessionId":null,"prompt":[]}`,
		`{}`,
		`null`,
		`[]`,
		`"s"`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var first acp.PromptRequest
		if err := json.Unmarshal(data, &first); err != nil {
			return // refused, which is an answer and not a failure
		}

		encoded, err := json.Marshal(&first)
		if err != nil {
			t.Fatalf("a value that decoded does not encode: %v", err)
		}

		var second acp.PromptRequest
		if decodeErr := json.Unmarshal(encoded, &second); decodeErr != nil {
			t.Fatalf("this package's own output does not decode: %v\n%s", decodeErr, encoded)
		}
		reencoded, reencodeErr := json.Marshal(&second)
		if reencodeErr != nil {
			t.Fatalf("re-encoding failed: %v", reencodeErr)
		}
		if string(encoded) != string(reencoded) {
			t.Fatalf("normalisation is not a fixed point:\n first %s\nsecond %s", encoded, reencoded)
		}
	})
}

// The same property for a type whose recovery is the busiest: two arrays that drop
// invalid items, one of them required and both falling back to an empty array.
func FuzzNewSessionRequestNormalisationIsAFixedPoint(f *testing.F) {
	seeds := []string{
		`{"cwd":"/w","mcpServers":[]}`,
		`{"cwd":"/w","mcpServers":"none"}`,
		`{"cwd":"/w","mcpServers":[{"name":"d","command":"c","args":[],"env":[]}]}`,
		`{"cwd":"/w","mcpServers":[{"type":"http","name":"h","url":"u","headers":[]}]}`,
		`{"cwd":"/w","mcpServers":[{"type":"quantum"}],"additionalDirectories":["/a",7]}`,
		`{"cwd":"/w","mcpServers":[],"additionalDirectories":3}`,
		`{"cwd":"/w"}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var first acp.NewSessionRequest
		if err := json.Unmarshal(data, &first); err != nil {
			return
		}
		encoded, err := json.Marshal(&first)
		if err != nil {
			t.Fatalf("a value that decoded does not encode: %v", err)
		}
		var second acp.NewSessionRequest
		if decodeErr := json.Unmarshal(encoded, &second); decodeErr != nil {
			t.Fatalf("this package's own output does not decode: %v\n%s", decodeErr, encoded)
		}
		reencoded, reencodeErr := json.Marshal(&second)
		if reencodeErr != nil {
			t.Fatalf("re-encoding failed: %v", reencodeErr)
		}
		if string(encoded) != string(reencoded) {
			t.Fatalf("normalisation is not a fixed point:\n first %s\nsecond %s", encoded, reencoded)
		}
	})
}

// A byte-stream framer must answer the same however the stream was split into
// reads: a pipe delivers a line in as many pieces as the kernel likes, and a
// framer whose result depended on where a read ended would decode one peer's
// message differently on every run.
func FuzzIOTransportFramingIgnoresReadBoundaries(f *testing.F) {
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"_a\"}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"), uint8(1))
	f.Add([]byte("\n\n{\"jsonrpc\":\"2.0\",\"method\":\"_a\"}"), uint8(3))
	f.Add([]byte("not json\n{\"jsonrpc\":\"1.0\",\"method\":\"_a\"}\n"), uint8(2))
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":1}}\r\n"), uint8(5))
	f.Fuzz(func(t *testing.T, stream []byte, chunk uint8) {
		whole := readFramed(t, stream, len(stream)+1)
		split := readFramed(t, stream, int(chunk)%16+1)
		if !slices.Equal(whole, split) {
			t.Fatalf("framing depends on read boundaries:\nwhole: %q\nsplit: %q", whole, split)
		}
	})
}

// readFramed reads every message from stream, delivered chunk bytes at a time,
// and describes each result: the re-encoded message, or the kind of error.
func readFramed(t *testing.T, stream []byte, chunk int) []string {
	t.Helper()
	transport := acp.NewIOTransport(&acp.IOConfig{
		Reader: &chunkedReader{data: stream, chunk: chunk},
		Writer: io.Discard,
		Close:  func() error { return nil },
	})
	conn, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for {
		message, err := conn.Read(t.Context())
		if decodeErr, ok := errors.AsType[*jsonrpc.DecodeError](err); ok {
			results = append(results, fmt.Sprintf("undecodable syntax=%t response=%s", decodeErr.Syntax,
				decodeErr.ResponseID))
			continue
		}
		if errors.Is(err, io.EOF) {
			return results
		}
		if err != nil {
			return append(results, "failed")
		}
		encoded, err := jsonrpc.EncodeMessage(message)
		if err != nil {
			t.Fatalf("a decoded message does not encode: %v", err)
		}
		results = append(results, string(encoded))
	}
}

// A chunkedReader returns at most chunk bytes per Read.
type chunkedReader struct {
	data  []byte
	chunk int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), r.chunk)], r.data)
	r.data = r.data[n:]
	return n, nil
}
