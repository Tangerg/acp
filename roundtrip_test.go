package acp_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The encoder's promise over every generated type, which is where a generator bug
// ships unnoticed: the fixture corpus reaches the encoder only where a case
// re-encodes, and the golden tests cover what somebody thought to write down.
//
// The zero value is the only value every type has without the schema knowledge a
// meaningful one would need. So this says nothing about how a populated value
// encodes — that is the fixtures' and the golden tests' half.
//
// The trap is the pass on which it settles. Decoding is deliberately not the
// inverse of encoding: an optional property with a declared default decodes to
// that default, so encoding a zero InitializeResponse omits authMethods and
// decoding materialises it as []. Both are correct, and what must hold is that
// applying the round trip again changes nothing.
func TestEveryGeneratedTypeNormalisesToAFixedPointOrRefuses(t *testing.T) {
	var encoded, refused int

	for _, generated := range generatedValues {
		t.Run(generated.Name, func(t *testing.T) {
			first, err := json.Marshal(generated.New())
			if err != nil {
				// A zero value is not always a legal message — a required enum has no
				// member spelled "" — so refusing is correct, and the only thing to
				// hold it to is that the refusal is this package's own rather than a
				// panic or a codec that got halfway.
				if !strings.Contains(err.Error(), "acp: ") {
					t.Fatalf("refused with %v, which is not this package saying why", err)
				}
				refused++
				return
			}

			if !json.Valid(first) {
				t.Fatalf("encoded %s, which is not JSON", first)
			}

			// Encodings rather than values: equality for a type holding interfaces
			// and unexported state would need a comparer of its own, and the bytes
			// are what the wire carries.
			second := renormalise(t, generated.New(), string(first))
			third := renormalise(t, generated.New(), second)
			if second != third {
				t.Fatalf("normalisation does not settle:\n first  %s\n second %s\n third  %s",
					first, second, third)
			}
			encoded++
		})
	}

	// The split is the schema's to decide, so it is reported rather than asserted.
	t.Logf("%d generated types encode a zero value and settle; %d refuse one",
		encoded, refused)

	if encoded == 0 {
		t.Fatal("no generated type round-tripped, so this proved nothing")
	}
}

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
