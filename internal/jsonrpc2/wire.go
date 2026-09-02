// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forked from golang.org/x/tools/internal/jsonrpc2_v2 at v0.49.0. The changes are
// listed in doc.go; in this file, upstream's non-standard error sentinels are
// removed, because two of their codes mean something else in the Agent Client
// Protocol.

package jsonrpc2

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file contains the Go forms of the JSON-RPC wire specification.

var (
	// ErrParse reports invalid JSON.
	ErrParse = NewError(-32700, "JSON-RPC parse error")
	// ErrInvalidRequest reports an invalid request envelope.
	ErrInvalidRequest = NewError(-32600, "JSON-RPC invalid request")
)

const wireVersion = "2.0"

// wireCombined is the shared encoding shape for Request and Response. Decoding
// uses an exact-key raw-member map because member presence determines the shape.
type wireCombined struct {
	VersionTag string          `json:"jsonrpc"`
	ID         ID              `json:"id,omitzero"`
	Method     *string         `json:"method,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *WireError      `json:"error,omitempty"`
}

// WireError represents a structured error in a Response.
type WireError struct {
	// Code is an error code indicating the type of failure.
	Code int64 `json:"code"`
	// Message is a short description of the error.
	Message string `json:"message"`
	// Data is optional structured data containing additional information about the error.
	Data json.RawMessage `json:"data,omitempty"`
}

func (err *WireError) UnmarshalJSON(data []byte) error {
	if err == nil {
		return errors.New("jsonrpc: decode error into nil target")
	}
	var members map[string]json.RawMessage
	if decodeErr := json.Unmarshal(data, &members); decodeErr != nil {
		return fmt.Errorf("jsonrpc: decode error: %w", decodeErr)
	}
	if members == nil {
		return errors.New("jsonrpc: decode error: expected an object")
	}

	var decoded WireError
	rawCode, hasCode := members["code"]
	if !hasCode {
		return errors.New("jsonrpc: decode error: missing code")
	}
	if decodeErr := json.Unmarshal(rawCode, &decoded.Code); decodeErr != nil {
		return fmt.Errorf("jsonrpc: decode error code: %w", decodeErr)
	}
	rawMessage, hasMessage := members["message"]
	if !hasMessage {
		return errors.New("jsonrpc: decode error: missing message")
	}
	if decodeErr := json.Unmarshal(rawMessage, &decoded.Message); decodeErr != nil {
		return fmt.Errorf("jsonrpc: decode error message: %w", decodeErr)
	}
	decoded.Data = members["data"]
	*err = decoded
	return nil
}

// NewError returns an error that will encode on the wire correctly.
// The standard codes are made available from this package, this function should
// only be used to build errors for application specific codes as allowed by the
// specification.
func NewError(code int64, message string) *WireError {
	return &WireError{
		Code:    code,
		Message: message,
	}
}

func (err *WireError) Error() string {
	return err.Message
}

func (err *WireError) Is(other error) bool {
	w, ok := other.(*WireError)
	if !ok {
		return false
	}
	return err.Code == w.Code
}
