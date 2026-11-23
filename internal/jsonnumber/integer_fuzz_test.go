package jsonnumber_test

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/jsonnumber"
)

// The rational oracle is independent of the decoder's decimal-place algorithm.
// Its exponent is an int8 so fuzzing cannot make the oracle expand huge powers.
func FuzzIntegerValues(f *testing.F) {
	f.Add(uint64(0), false, uint8(0), int8(0))
	f.Add(uint64(math.MaxInt64), false, uint8(0), int8(0))
	f.Add(uint64(math.MaxInt64)+1, true, uint8(0), int8(0))
	f.Add(uint64(math.MaxUint64), false, uint8(2), int8(2))
	f.Add(uint64(1000), true, uint8(5), int8(2))
	f.Fuzz(func(t *testing.T, coefficient uint64, negative bool, fraction uint8, exponent int8) {
		digits := strconv.FormatUint(coefficient, 10)
		places := int(fraction)
		if places > 0 {
			if len(digits) <= places {
				digits = strings.Repeat("0", places-len(digits)+1) + digits
			}
			split := len(digits) - places
			digits = digits[:split] + "." + digits[split:]
		}
		if negative {
			digits = "-" + digits
		}
		input := digits + "e" + strconv.FormatInt(int64(exponent), 10)
		oracle, ok := new(big.Rat).SetString(input)
		if !ok {
			t.Fatalf("oracle could not parse generated input %q", input)
		}
		signed, signedErr := jsonnumber.Parse[int64]([]byte(input))
		wantSigned := oracle.IsInt() && oracle.Num().IsInt64()
		if (signedErr == nil) != wantSigned || wantSigned && signed != oracle.Num().Int64() {
			t.Fatalf("Parse[int64](%q) = %d, %v; rational = %v", input, signed, signedErr, oracle)
		}
		unsigned, unsignedErr := jsonnumber.Parse[uint64]([]byte(input))
		wantUnsigned := oracle.IsInt() && oracle.Num().IsUint64()
		if (unsignedErr == nil) != wantUnsigned || wantUnsigned && unsigned != oracle.Num().Uint64() {
			t.Fatalf("Parse[uint64](%q) = %d, %v; rational = %v", input, unsigned, unsignedErr, oracle)
		}
	})
}
