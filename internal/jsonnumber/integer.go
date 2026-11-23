// Package jsonnumber decodes JSON integers without rounding or expanding exponents.
package jsonnumber

import (
	"bytes"
	"errors"
)

// Integer permits named integer types without forcing a lossy float conversion.
type Integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

var (
	// ErrSyntax distinguishes malformed JSON from a valid number outside the
	// requested integer domain.
	ErrSyntax = errors.New("invalid JSON number")
	// ErrNotInteger reports a fractional value, which must never be rounded by
	// an integer decoder.
	ErrNotInteger = errors.New("JSON number is not an integer")
	// ErrRange distinguishes an integral value that narrowing to the requested
	// Go type would change.
	ErrRange = errors.New("JSON integer is out of range")
)

const (
	decimalRadix       = uint64(10)
	maxMagnitude       = ^uint64(0)
	maxMagnitudeDigits = 20
)

// Parse checks the mathematical integer value before narrowing to T. In
// particular, exponent size never determines allocation or iteration count.
func Parse[T Integer](data []byte) (T, error) {
	var zero T
	number, err := parseDecimal(data)
	if err != nil {
		return zero, err
	}
	magnitude, err := number.magnitude()
	if err != nil {
		return zero, err
	}
	value := T(magnitude)
	if number.negative && magnitude != 0 {
		value = -value
		// Adding one before negation also represents the minimum signed value,
		// whose positive counterpart cannot be held by T.
		if value >= 0 || uint64(-(value+1))+1 != magnitude {
			return zero, ErrRange
		}
	} else if value < 0 || uint64(value) != magnitude {
		return zero, ErrRange
	}
	return value, nil
}

type decimal struct {
	significand      []byte
	fractionalDigits int
	exponent         uint64
	exponentNegative bool
	negative         bool
}

func parseDecimal(data []byte) (decimal, error) {
	data = bytes.Trim(data, " \t\r\n")
	number := decimal{}
	index := 0
	if len(data) > 0 && data[0] == '-' {
		number.negative = true
		index++
	}
	start := index
	if index == len(data) {
		return number, ErrSyntax
	}
	switch {
	case data[index] == '0':
		index++
	case data[index] >= '1' && data[index] <= '9':
		index = digitsEnd(data, index)
	default:
		return number, ErrSyntax
	}
	if index < len(data) && data[index] == '.' {
		fraction := index + 1
		index = digitsEnd(data, fraction)
		if index == fraction {
			return number, ErrSyntax
		}
		number.fractionalDigits = index - fraction
	}
	number.significand = data[start:index]
	if index < len(data) && (data[index] == 'e' || data[index] == 'E') {
		index++
		if index < len(data) && (data[index] == '-' || data[index] == '+') {
			number.exponentNegative = data[index] == '-'
			index++
		}
		start = index
		index = digitsEnd(data, start)
		if index == start {
			return number, ErrSyntax
		}
		for _, digit := range data[start:index] {
			value := uint64(digit - '0')
			if number.exponent > (maxMagnitude-value)/decimalRadix {
				// No input slice can contain enough fractional places to bring an
				// exponent this large back into an integer type's range.
				number.exponent = maxMagnitude
				continue
			}
			number.exponent = number.exponent*decimalRadix + value
		}
	}
	if index != len(data) {
		return number, ErrSyntax
	}
	return number, nil
}

func digitsEnd(data []byte, index int) int {
	for index < len(data) && data[index] >= '0' && data[index] <= '9' {
		index++
	}
	return index
}

func (d decimal) magnitude() (uint64, error) {
	first, last := -1, -1
	digits, leading, trailing := 0, 0, 0
	for index, digit := range d.significand {
		if digit == '.' {
			continue
		}
		digits++
		if digit == '0' {
			trailing++
			continue
		}
		if first < 0 {
			first = index
			leading = digits - 1
		}
		last = index
		trailing = 0
	}
	if first < 0 {
		return 0, nil
	}
	// Removing trailing zeroes before applying the exponent makes integrality
	// a decimal-place comparison, even when the coefficient itself is enormous.
	power, err := d.integralPower(trailing)
	if err != nil {
		return 0, err
	}
	significantDigits := digits - leading - trailing
	if significantDigits > maxMagnitudeDigits || power > uint64(maxMagnitudeDigits-significantDigits) {
		return 0, ErrRange
	}
	var magnitude uint64
	for _, digit := range d.significand[first : last+1] {
		if digit == '.' {
			continue
		}
		value := uint64(digit - '0')
		if magnitude > (maxMagnitude-value)/decimalRadix {
			return 0, ErrRange
		}
		magnitude = magnitude*decimalRadix + value
	}
	for range power {
		if magnitude > maxMagnitude/decimalRadix {
			return 0, ErrRange
		}
		magnitude *= decimalRadix
	}
	return magnitude, nil
}

func (d decimal) integralPower(trailingZeros int) (uint64, error) {
	shift := trailingZeros - d.fractionalDigits
	if d.exponentNegative {
		if shift < 0 || d.exponent > uint64(shift) {
			return 0, ErrNotInteger
		}
		return uint64(shift) - d.exponent, nil
	}
	if shift < 0 {
		if d.exponent < uint64(-shift) {
			return 0, ErrNotInteger
		}
		return d.exponent - uint64(-shift), nil
	}
	if d.exponent > maxMagnitudeDigits || uint64(shift) > maxMagnitudeDigits-d.exponent {
		return 0, ErrRange
	}
	return d.exponent + uint64(shift), nil
}
