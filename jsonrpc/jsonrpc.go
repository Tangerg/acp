// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package jsonrpc is the JSON-RPC 2.0 message layer: the message types a custom
// ACP transport carries and the codec a byte-stream transport frames. Request
// lifecycle and method dispatch belong to package acp.
//
// The envelope handling descends from golang.org/x/tools/internal/jsonrpc2_v2 at
// v0.49.0 under the Go Authors' BSD licence in this directory. It departs from
// upstream where the Agent Client Protocol narrows JSON-RPC: request identifiers
// keep explicit null apart from absence and accept only int64 numbers, envelope
// members are matched exactly, and an error object is carried as validated JSON
// whose grammar package acp owns.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/Tangerg/acp/internal/jsonnumber"
)

// An ID is a request identifier: a string, a signed 64-bit integer, or explicit
// null. Its zero value is the absent identifier of a notification.
type ID struct {
	kind   idKind
	number int64
	text   string
}

type idKind uint8

const (
	idAbsent idKind = iota
	idNull
	idNumber
	idString
)

// StringID returns a string request identifier.
func StringID(s string) ID { return ID{kind: idString, text: s} }

// Int64ID returns an integer request identifier.
func Int64ID(i int64) ID { return ID{kind: idNumber, number: i} }

// NullID returns the discouraged but valid null identifier.
func NullID() ID { return ID{kind: idNull} }

// IsValid reports whether the identifier is present, as null or as a value.
func (id ID) IsValid() bool { return id.kind != idAbsent }

func (id ID) String() string {
	switch id.kind {
	case idNull:
		return "null"
	case idNumber:
		return strconv.FormatInt(id.number, 10)
	case idString:
		return strconv.Quote(id.text)
	default:
		return "absent"
	}
}

func (id ID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case idNumber:
		return strconv.AppendInt(nil, id.number, 10), nil
	case idString:
		return json.Marshal(id.text)
	default:
		return []byte("null"), nil
	}
}

func (id *ID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*id = NullID()
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*id = StringID(text)
		return nil
	}
	number, err := jsonnumber.Parse[int64](data)
	if err != nil {
		// The identifier is the peer's and may be any size, so it is not echoed.
		return errors.New("jsonrpc: request ID is not a string, null, or int64")
	}
	*id = Int64ID(number)
	return nil
}

// A Message is a JSON-RPC request or response. The set is closed: the concrete
// types are [*Request] and [*Response].
type Message interface {
	isMessage()
}

// A Request is a message sent to a peer. One with an identifier is a call and
// expects a response; one without is a notification.
type Request struct {
	ID     ID
	Method string
	// Params is the raw params member, or nil when it is absent.
	Params json.RawMessage
}

// A Response answers a call with exactly one of a result and an error.
type Response struct {
	ID     ID
	Result json.RawMessage
	// Error is the raw error object. Its grammar is the protocol's, not the
	// envelope's, so it is validated here only as a JSON object.
	Error json.RawMessage
}

func (*Request) isMessage()  {}
func (*Response) isMessage() {}

// IsCall reports whether the request expects a response.
func (r *Request) IsCall() bool { return r.ID.IsValid() }

func (r *Response) validate() error {
	switch {
	case r == nil:
		return errors.New("nil response")
	case !r.ID.IsValid():
		return errors.New("response has no request id")
	case r.Result != nil && len(r.Result) == 0:
		return errors.New("response result is empty JSON")
	case r.Error != nil && len(r.Result) > 0:
		return errors.New("response has both result and error")
	case r.Error == nil && len(r.Result) == 0:
		return errors.New("response has neither result nor error")
	case r.Error != nil && firstByte(r.Error) != '{':
		return errors.New("response error is not an object")
	default:
		return nil
	}
}

const wireVersion = "2.0"

// wireEnvelope is the encoded shape shared by requests and responses.
type wireEnvelope struct {
	Version string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  *string         `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// EncodeMessage encodes one message without transport framing, refusing an
// envelope that breaks JSON-RPC's obligations.
func EncodeMessage(message Message) ([]byte, error) {
	envelope := wireEnvelope{Version: wireVersion}
	switch message := message.(type) {
	case *Request:
		if message == nil {
			return nil, errors.New("jsonrpc: encode message: nil request")
		}
		if message.Params != nil && len(message.Params) == 0 {
			return nil, errors.New("jsonrpc: encode message: params is empty JSON")
		}
		if message.IsCall() {
			envelope.ID = &message.ID
		}
		envelope.Method = &message.Method
		envelope.Params = message.Params
	case *Response:
		if err := message.validate(); err != nil {
			return nil, fmt.Errorf("jsonrpc: encode message: %w", err)
		}
		envelope.ID = &message.ID
		envelope.Result = message.Result
		envelope.Error = message.Error
	default:
		return nil, fmt.Errorf("jsonrpc: encode message: unsupported type %T", message)
	}
	data, err := json.Marshal(&envelope)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: encode message: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("jsonrpc: encode message: invalid UTF-8")
	}
	return data, nil
}

// A DecodeError reports one message that could not be decoded. Framing is
// intact, so a connection answers it and keeps reading.
type DecodeError struct {
	// Syntax reports bytes that are not JSON, rather than JSON that is not a
	// valid message.
	Syntax bool
	// ResponseID identifies a malformed response whose identifier was readable,
	// so the call it answers can fail instead of waiting. It is absent otherwise.
	ResponseID ID
	Err        error
}

func (e *DecodeError) Error() string { return "jsonrpc: decode message: " + e.Err.Error() }

func (e *DecodeError) Unwrap() error { return e.Err }

// DecodeMessage decodes one message, refusing a malformed envelope with a
// [*DecodeError].
func DecodeMessage(data []byte) (Message, error) {
	if !utf8.Valid(data) {
		return nil, &DecodeError{Syntax: true, Err: errors.New("invalid UTF-8")}
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		var syntaxErr *json.SyntaxError
		return nil, &DecodeError{Syntax: errors.As(err, &syntaxErr), Err: err}
	}
	if members == nil {
		return nil, &DecodeError{Err: errors.New("the message is not an object")}
	}

	var version string
	if err := json.Unmarshal(members["jsonrpc"], &version); err != nil || version != wireVersion {
		return nil, &DecodeError{Err: fmt.Errorf("jsonrpc member is not %q", wireVersion)}
	}

	var id ID
	if rawID, present := members["id"]; present {
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, &DecodeError{Err: err}
		}
	}

	// Presence, not a non-empty value, distinguishes a request from a response.
	if rawMethod, hasMethod := members["method"]; hasMethod {
		var method string
		if err := json.Unmarshal(rawMethod, &method); err != nil || firstByte(rawMethod) == 'n' {
			return nil, &DecodeError{Err: errors.New("method is not a string")}
		}
		return &Request{ID: id, Method: method, Params: members["params"]}, nil
	}
	if !id.IsValid() {
		return nil, &DecodeError{Err: errors.New("a response without an id")}
	}
	response := &Response{ID: id, Result: members["result"]}
	if rawError, hasError := members["error"]; hasError {
		response.Error = rawError
	}
	if err := response.validate(); err != nil {
		return nil, &DecodeError{ResponseID: id, Err: err}
	}
	return response, nil
}

func firstByte(data []byte) byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}
