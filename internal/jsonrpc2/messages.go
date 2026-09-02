// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forked from golang.org/x/tools/internal/jsonrpc2_v2 at v0.49.0. The changes are
// listed in doc.go; in this file, EncodeIndent is removed.

package jsonrpc2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// ID is a JSON-RPC request identifier: a string, integer, or null.
type ID struct {
	value any
	valid bool
}

func (id ID) MarshalJSON() ([]byte, error) {
	if !id.valid || id.value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(id.value)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return fmt.Errorf("%w: decode request ID into nil target", ErrParse)
	}
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
	number, ok := new(big.Rat).SetString(string(data))
	if !ok || !number.IsInt() || !number.Num().IsInt64() {
		return fmt.Errorf("%w: request ID %s is not a string, null, or int64", ErrParse, data)
	}
	*id = Int64ID(number.Num().Int64())
	return nil
}

// Message is the interface to all jsonrpc2 message types.
// They share no common functionality, but are a closed set of concrete types
// that are allowed to implement this interface. The message types are *Request
// and *Response.
type Message interface {
	// marshal builds the wire form from the API form.
	// It is private, which makes the set of Message implementations closed.
	marshal(to *wireCombined)
}

// Request is a Message sent to a peer to request behavior.
// If it has an ID it is a call, otherwise it is a notification.
type Request struct {
	// ID of this request, used to tie the Response back to the request.
	// This is the zero ID for notifications; an explicit null is valid and
	// distinct from that zero.
	ID ID
	// Method is a string containing the method name to invoke.
	Method string
	// Params is either a struct or an array with the parameters of the method.
	Params json.RawMessage
}

// Response is a Message used as a reply to a call Request.
// It will have the same ID as the call it is a response to.
type Response struct {
	// Result is the content of the response.
	Result json.RawMessage
	// Error is set only if the call failed.
	Error *WireError
	// ID is the identifier of the request this answers.
	ID ID
}

// StringID creates a new string request identifier.
func StringID(s string) ID { return ID{value: s, valid: true} }

// Int64ID creates a new integer request identifier.
func Int64ID(i int64) ID { return ID{value: i, valid: true} }

// NullID creates the discouraged but valid null arm of ACP's RequestId union.
func NullID() ID { return ID{valid: true} }

// IsValid returns true if the ID is a valid identifier.
// The default value for ID will return false.
func (id ID) IsValid() bool { return id.valid }

// IsZero keeps an absent ID out of an envelope while allowing an explicit null
// ID to remain present, as the ACP RequestId union requires.
func (id ID) IsZero() bool { return !id.valid }

// Raw returns the underlying value of the ID.
func (id ID) Raw() any { return id.value }

// NewNotification constructs a new Notification message for the supplied
// method and parameters.
func NewNotification(method string, params any) (*Request, error) {
	p, merr := marshalToRaw(params)
	return &Request{Method: method, Params: p}, merr
}

// NewCall constructs a new Call message for the supplied ID, method and
// parameters.
func NewCall(id ID, method string, params any) (*Request, error) {
	if !id.IsValid() {
		return nil, errors.New("jsonrpc: construct call: request has no id")
	}
	p, merr := marshalToRaw(params)
	return &Request{ID: id, Method: method, Params: p}, merr
}

func (msg *Request) IsCall() bool { return msg.ID.IsValid() }

func (msg *Request) marshal(to *wireCombined) {
	to.ID = msg.ID
	to.Method = &msg.Method
	to.Params = msg.Params
}

// NewResponse constructs a response with exactly one of result or rerr.
func NewResponse(id ID, result any, rerr *WireError) (*Response, error) {
	if !id.IsValid() {
		return nil, errors.New("jsonrpc: construct response: response has no request id")
	}
	r, err := marshalToRaw(result)
	if err != nil {
		return nil, err
	}
	response := &Response{ID: id, Result: r, Error: rerr}
	if err := response.validate(); err != nil {
		return nil, fmt.Errorf("jsonrpc: construct response: %w", err)
	}
	return response, nil
}

func (msg *Response) marshal(to *wireCombined) {
	to.ID = msg.ID
	to.Error = msg.Error
	to.Result = msg.Result
}

func (msg *Response) validate() error {
	switch {
	case msg == nil:
		return errors.New("nil response")
	case !msg.ID.IsValid():
		return errors.New("response has no request id")
	case msg.Error != nil && len(msg.Result) > 0:
		return errors.New("response has both result and error")
	case msg.Error == nil && len(msg.Result) == 0:
		return errors.New("response has neither result nor error")
	default:
		return nil
	}
}

func EncodeMessage(msg Message) ([]byte, error) {
	switch msg := msg.(type) {
	case *Request:
		if msg == nil {
			return nil, errors.New("jsonrpc: marshal message: nil request")
		}
	case *Response:
		if err := msg.validate(); err != nil {
			return nil, fmt.Errorf("jsonrpc: marshal message: %w", err)
		}
	default:
		return nil, fmt.Errorf("jsonrpc: marshal message: unsupported type %T", msg)
	}
	wire := wireCombined{VersionTag: wireVersion}
	msg.marshal(&wire)
	data, err := json.Marshal(&wire)
	if err != nil {
		return data, fmt.Errorf("jsonrpc: marshal message: %w", err)
	}
	return data, nil
}

func DecodeMessage(data []byte) (Message, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, fmt.Errorf("jsonrpc: unmarshal message: %w", err)
	}
	if members == nil {
		return nil, ErrInvalidRequest
	}

	var version string
	if err := json.Unmarshal(members["jsonrpc"], &version); err != nil {
		return nil, ErrInvalidRequest
	}
	if version != wireVersion {
		return nil, fmt.Errorf("jsonrpc: invalid version %q; want %q", version, wireVersion)
	}

	var id ID
	if rawID, present := members["id"]; present {
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, fmt.Errorf("jsonrpc: unmarshal message: %w", err)
		}
	}

	if rawMethod, hasMethod := members["method"]; hasMethod {
		// Presence, not a non-empty value, distinguishes requests from responses.
		// An empty method is still a request for the dispatcher to reject; treating
		// it as a response would reinterpret the peer's envelope.
		var method string
		if err := json.Unmarshal(rawMethod, &method); err != nil || bytes.Equal(bytes.TrimSpace(rawMethod), []byte("null")) {
			return nil, ErrInvalidRequest
		}
		return &Request{
			Method: method,
			ID:     id,
			Params: members["params"],
		}, nil
	}
	if !id.IsValid() {
		return nil, ErrInvalidRequest
	}

	var wireErr *WireError
	rawError, hasError := members["error"]
	if hasError {
		if bytes.Equal(bytes.TrimSpace(rawError), []byte("null")) {
			return nil, errors.New("jsonrpc: invalid response: error is null")
		}
		wireErr = new(WireError)
		if err := json.Unmarshal(rawError, wireErr); err != nil {
			return nil, fmt.Errorf("jsonrpc: invalid response: %w", err)
		}
	}
	resp := &Response{
		ID:     id,
		Result: members["result"],
		Error:  wireErr,
	}
	if err := resp.validate(); err != nil {
		return nil, fmt.Errorf("jsonrpc: invalid response: %w", err)
	}
	return resp, nil
}

func marshalToRaw(obj any) (json.RawMessage, error) {
	if obj == nil {
		return nil, nil
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
