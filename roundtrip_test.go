package acp_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The encoder's promise, held against every generated type rather than the
// handful a fixture happens to name.
//
// The fixture corpus starts from JSON a peer sent, so it exercises the decoder
// across the type surface and the encoder only where a case re-encodes. The
// golden tests state exact bytes for values built in Go, but they are written by
// hand and therefore cover what somebody thought to write down. Neither reaches
// the encoder of a type nobody has needed yet — and a type nobody has needed yet
// is precisely where a generator bug ships unnoticed.
//
// The property is stated over the zero value because it is the one value every
// generated type has without the schema knowledge that constructing a meaningful
// one would need. That is a real limit: this says nothing about how a populated
// value encodes, which is what the fixtures and the golden tests are for. What it
// does say is that no generated codec emits something it cannot read back, and
// that the ones which cannot encode a zero value refuse rather than emit
// nonsense.
//
// # Why the fixed point is reached on the second pass and not the first
//
// Decoding is deliberately not the inverse of encoding. The schema gives some
// optional properties a declared default — InitializeResponse.authMethods is an
// optional array defaulting to [] — so an absent property does not decode to
// absent, it decodes to the default. Encoding a zero value omits it and decoding
// that materialises it, and the two are both correct.
//
// What must hold is that this settles: applying the round trip again changes
// nothing. That is the same promise the fuzz targets make about a peer's bytes,
// asserted here across every generated type instead of across whatever the corpus
// reached.
func TestEveryGeneratedTypeNormalisesToAFixedPointOrRefuses(t *testing.T) {
	var encoded, refused int

	for _, generated := range generatedValues {
		t.Run(generated.Name, func(t *testing.T) {
			first, err := json.Marshal(generated.New())
			if err != nil {
				// The second branch. A zero value is not always a legal message: a
				// required enum has no member spelled "" and a required union has no
				// arm selected, and the schema says both are wrong. Refusing is the
				// correct answer, and the only thing to hold it to is that the refusal
				// is this package's own rather than a panic or a codec that got
				// halfway.
				if !strings.Contains(err.Error(), "acp: ") {
					t.Fatalf("refused with %v, which is not this package saying why", err)
				}
				refused++
				return
			}

			if !json.Valid(first) {
				t.Fatalf("encoded %s, which is not JSON", first)
			}

			// Comparing the encodings rather than the values: equality for a type
			// holding interfaces and unexported state would need a comparer of its
			// own, and the bytes are what the wire actually carries.
			second := renormalise(t, generated.New(), string(first))
			third := renormalise(t, generated.New(), second)
			if second != third {
				t.Fatalf("normalisation does not settle:\n first  %s\n second %s\n third  %s",
					first, second, third)
			}
			encoded++
		})
	}

	// Not an assertion on the split, which is the schema's to decide, but the
	// number a reader needs to judge what the run above actually proved.
	t.Logf("%d generated types encode a zero value and settle; %d refuse one",
		encoded, refused)

	if encoded == 0 {
		t.Fatal("no generated type round-tripped, so this proved nothing")
	}
}

// renormalise decodes one encoding and encodes what it decoded, which is one
// application of the round trip the property is about.
func renormalise(t *testing.T, into any, encoded string) string {
	t.Helper()

	if err := json.Unmarshal([]byte(encoded), into); err != nil {
		t.Fatalf("could not read back %s: %v", encoded, err)
	}
	again, err := json.Marshal(into)
	if err != nil {
		t.Fatalf("re-encoding what it had just decoded failed: %v", err)
	}
	return string(again)
}
