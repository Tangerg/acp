package wire

import (
	"encoding/json"
	"fmt"
	"math"
	"unicode/utf8"
)

// StringConstraints keeps encoding and decoding on the same schema constraints;
// a value accepted from a peer must also be one this side can encode.
type StringConstraints struct {
	MinLength   uint64
	AbsoluteURI bool
}

// Validate counts Unicode code points, as JSON Schema requires, rather than
// storage bytes or display graphemes.
func (c StringConstraints) Validate(value string) error {
	if c.MinLength > math.MaxInt || utf8.RuneCountInString(value) < int(c.MinLength) {
		return fmt.Errorf("acp: string has fewer than %d characters", c.MinLength)
	}
	if !c.AbsoluteURI {
		return nil
	}
	return ValidateURI(value)
}

// Decode rejects null before string validation, because encoding/json would
// otherwise turn it into an admissible empty string.
func (c StringConstraints) Decode(data []byte) (string, error) {
	value, err := UnmarshalValue[string](data)
	if err != nil {
		return "", err
	}
	if validationErr := c.Validate(value); validationErr != nil {
		return "", validationErr
	}
	return value, nil
}

// Encode checks constraints before writing, so local construction cannot bypass
// rules applied to peer input.
func (c StringConstraints) Encode(value string) ([]byte, error) {
	if err := c.Validate(value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
