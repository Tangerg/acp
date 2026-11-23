package jsonnumber_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/jsonnumber"
)

func TestIntegerRepresentations(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"-0.0e-1000000", 0},
		{"0e999999999999999999999999999999999999999999999999999", 0},
		{"1", 1},
		{"1.0", 1},
		{"1e0", 1},
		{"1E+0", 1},
		{"10e-1", 1},
		{"1.2300e2", 123},
		{"  -0.00123e+5\r\n", -123},
		{"9223372036854775807", math.MaxInt64},
		{"92233720368547758070e-1", math.MaxInt64},
		{"9.223372036854775807e18", math.MaxInt64},
		{"-9223372036854775808.00000", math.MinInt64},
		{"-9.223372036854775808e18", math.MinInt64},
		{"1" + strings.Repeat("0", 1000) + "e-1000", 1},
		{"0." + strings.Repeat("0", 999) + "1e1000", 1},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := jsonnumber.Parse[int64]([]byte(test.input))
			if err != nil || got != test.want {
				t.Fatalf("Parse(%q) = %d, %v; want %d", test.input, got, err, test.want)
			}
		})
	}
}

func TestIntegerSyntaxAndIntegrality(t *testing.T) {
	for _, input := range []string{
		"", "-", "+1", "01", "-01", ".1", "1.", "1e", "1e+", "1e-", "1e1e1",
		"1 0", "1/1", "0x1", "NaN", "Infinity", "true", "null", `"1"`, "[]", "{}", "\u00a01",
	} {
		if _, err := jsonnumber.Parse[int64]([]byte(input)); !errors.Is(err, jsonnumber.ErrSyntax) {
			t.Errorf("Parse(%q) = %v; want syntax error", input, err)
		}
	}
	for _, input := range []string{"0.1", "1.2", "10e-2", "-1.00000000000000001", "1e-1000000"} {
		if _, err := jsonnumber.Parse[int64]([]byte(input)); !errors.Is(err, jsonnumber.ErrNotInteger) {
			t.Errorf("Parse(%q) = %v; want non-integer error", input, err)
		}
	}
}

func TestIntegerRanges(t *testing.T) {
	checkRange[int8](t, "-128", "127", "-129", "128")
	checkRange[int16](t, "-32768", "32767", "-32769", "32768")
	checkRange[int32](t, "-2147483648", "2147483647", "-2147483649", "2147483648")
	checkRange[int64](t, "-9223372036854775808", "9223372036854775807", "-9223372036854775809", "9223372036854775808")
	checkRange[uint8](t, "0", "255", "-1", "256")
	checkRange[uint16](t, "0", "65535", "-1", "65536")
	checkRange[uint32](t, "0", "4294967295", "-1", "4294967296")
	checkRange[uint64](t, "0", "18446744073709551615", "-1", "18446744073709551616")

	for _, input := range []string{"-0", "-0.0e1000000"} {
		if value, err := jsonnumber.Parse[uint64]([]byte(input)); err != nil || value != 0 {
			t.Errorf("Parse[uint64](%q) = %d, %v; want 0", input, value, err)
		}
	}
	value, err := jsonnumber.Parse[uint64]([]byte("1.8446744073709551615e19"))
	if err != nil || value != math.MaxUint64 {
		t.Fatalf("maximum uint64 exponent form = %d, %v", value, err)
	}
}

func checkRange[T jsonnumber.Integer](t *testing.T, minimum, maximum, below, above string) {
	t.Helper()
	for _, input := range []string{minimum, maximum} {
		for _, suffix := range []string{"", ".0", "e0"} {
			if _, err := jsonnumber.Parse[T]([]byte(input + suffix)); err != nil {
				t.Errorf("Parse[%T](%q) = %v", *new(T), input+suffix, err)
			}
		}
	}
	for _, input := range []string{below, above} {
		if _, err := jsonnumber.Parse[T]([]byte(input)); !errors.Is(err, jsonnumber.ErrRange) {
			t.Errorf("Parse[%T](%q) = %v; want range error", *new(T), input, err)
		}
	}
}

func TestLargeExponentDoesNotExpand(t *testing.T) {
	for _, input := range [][]byte{
		[]byte("1e1000000"),
		[]byte("1e18446744073709551615"),
		[]byte("1e99999999999999999999999999999999999999999999999999999999"),
	} {
		if _, err := jsonnumber.Parse[int64](input); !errors.Is(err, jsonnumber.ErrRange) {
			t.Fatalf("Parse(%q) = %v; want range error", input, err)
		}
		allocations := testing.AllocsPerRun(100, func() {
			_, _ = jsonnumber.Parse[int64](input)
		})
		if allocations != 0 {
			t.Errorf("Parse(%q) allocated %.0f objects", input, allocations)
		}
	}
}

func BenchmarkIntegerDecoding(b *testing.B) {
	for _, input := range []string{"1", "1e0", "9223372036854775807", "1e1000000"} {
		b.Run(input, func(b *testing.B) {
			data := []byte(input)
			b.ReportAllocs()
			for b.Loop() {
				_, _ = jsonnumber.Parse[int64](data)
			}
		})
	}
}
