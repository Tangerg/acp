// Package jsontest compares JSON documents by the values they denote, for tests
// that hold an encoding against one another implementation produced.
package jsontest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
)

// Decode decodes one JSON document with every number exact, as a *big.Rat, so
// that values float64 would merge — adjacent integers beyond 2^53 — stay apart.
func Decode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("jsontest: data continues after the document")
	}
	return exactNumbers(value)
}

// Equivalent reports whether a and b denote the same JSON value. Key order,
// whitespace and number spelling do not matter; numeric value does.
func Equivalent(a, b []byte) (bool, error) {
	x, err := Decode(a)
	if err != nil {
		return false, err
	}
	y, err := Decode(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

func exactNumbers(value any) (any, error) {
	switch current := value.(type) {
	case json.Number:
		number, valid := new(big.Rat).SetString(string(current))
		if !valid {
			return nil, fmt.Errorf("jsontest: cannot represent the number %q exactly", current)
		}
		return number, nil
	case []any:
		for index, item := range current {
			exact, err := exactNumbers(item)
			if err != nil {
				return nil, err
			}
			current[index] = exact
		}
	case map[string]any:
		for name, item := range current {
			exact, err := exactNumbers(item)
			if err != nil {
				return nil, err
			}
			current[name] = exact
		}
	}
	return value, nil
}
