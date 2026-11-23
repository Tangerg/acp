package jsontest_test

import (
	"testing"

	"github.com/Tangerg/acp/internal/jsontest"
)

func TestEquivalenceIsAboutValuesNotSpelling(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":[true,null]}`, `{ "b": [true, null], "a": 1.0 }`, true},
		{`1e2`, `100`, true},
		// float64 would merge these: both round to 2^53.
		{`9007199254740993`, `9007199254740992`, false},
		{`{"a":"1"}`, `{"a":1}`, false},
	} {
		got, err := jsontest.Equivalent([]byte(tc.a), []byte(tc.b))
		if err != nil {
			t.Fatalf("Equivalent(%s, %s): %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Errorf("Equivalent(%s, %s) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDataAfterTheDocumentIsRefused(t *testing.T) {
	if _, err := jsontest.Decode([]byte(`{} {}`)); err == nil {
		t.Fatal("a second document was ignored")
	}
}
