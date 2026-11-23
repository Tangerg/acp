package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A Manifest is the committed root set. Generation scope is everything the
// planner reaches from it.
type Manifest struct {
	Payloads []string
	Plumbing []string
	// Internal names the definitions that are generated but not exported, each
	// with the reason.
	Internal map[string]string
	// Bindings name definitions whose grammar a hand-written Go type already owns,
	// so the generator emits no second representation of it.
	Bindings map[string]*Binding
	// Projections are the handle-facing params types: a wire request minus the
	// identifiers a handle owns.
	Projections map[string]*ProjectionSpec
	// Probes are wire-shape witnesses no payload reaches yet. Reaching one from
	// the other roots is a generation failure, because the probe has done its job.
	Probes map[string]string
}

// A Binding maps a schema definition onto a Go type outside the generated file.
type Binding struct {
	// GoType is the qualified Go type, for example jsonrpc.ID.
	GoType string `json:"go"`
	// Shape is the definition shape the Go type is known to own. The planner
	// refuses a definition that stopped having it.
	Shape bindingShape `json:"shape"`
	Why   string       `json:"why"`
}

// A bindingShape is a definition shape a bound Go type can be held to.
type bindingShape string

// shapeJSONRPCID is null, a signed 64-bit integer or a string: JSON-RPC's id.
const shapeJSONRPCID bindingShape = "jsonrpc-id"

// A ProjectionSpec derives one params type from one request type.
type ProjectionSpec struct {
	From string `json:"from"`
	// Without names the properties the handle supplies, which the projection
	// therefore does not carry.
	Without []string `json:"without"`
	Why     string   `json:"why"`
}

// commentKey is how a bucket of the manifest explains itself.
const commentKey = "$comment"

func (m *Manifest) UnmarshalJSON(data []byte) error {
	var raw struct {
		Comment     string                     `json:"$comment"`
		Payloads    []string                   `json:"payloads"`
		Plumbing    []string                   `json:"plumbing"`
		Internal    map[string]json.RawMessage `json:"internal"`
		Bindings    map[string]json.RawMessage `json:"bindings"`
		Projections map[string]json.RawMessage `json:"projections"`
		Probes      map[string]json.RawMessage `json:"probes"`
	}
	if err := strictDecode(data, &raw); err != nil {
		return err
	}
	internal, err := decodeBucket[string]("internal", raw.Internal)
	if err != nil {
		return err
	}
	bindings, err := decodeBucket[*Binding]("bindings", raw.Bindings)
	if err != nil {
		return err
	}
	projections, err := decodeBucket[*ProjectionSpec]("projections", raw.Projections)
	if err != nil {
		return err
	}
	probes, err := decodeBucket[string]("probes", raw.Probes)
	if err != nil {
		return err
	}
	*m = Manifest{
		Payloads:    raw.Payloads,
		Plumbing:    raw.Plumbing,
		Internal:    internal,
		Bindings:    bindings,
		Projections: projections,
		Probes:      probes,
	}
	return m.validate()
}

func (m *Manifest) validate() error {
	for _, name := range sortedKeys(m.Bindings) {
		binding := m.Bindings[name]
		if binding.GoType == "" || binding.Why == "" {
			return fmt.Errorf("binding %s needs both a Go type and a reason", name)
		}
		if binding.Shape != shapeJSONRPCID {
			return fmt.Errorf("binding %s declares the unimplemented shape %q", name, binding.Shape)
		}
	}
	for _, name := range sortedKeys(m.Projections) {
		if m.Projections[name].Why == "" {
			return fmt.Errorf("projection %s has no stated reason; it has to say which handle owns the identifiers", name)
		}
	}
	return nil
}

// decodeBucket decodes one object bucket, dropping its comment key.
func decodeBucket[V any](bucket string, entries map[string]json.RawMessage) (map[string]V, error) {
	decoded := make(map[string]V, len(entries))
	for _, name := range sortedKeys(entries) {
		if name == commentKey {
			continue
		}
		if strings.HasPrefix(name, "$") {
			return nil, fmt.Errorf("%s.%s: only %s may start with $", bucket, name, commentKey)
		}
		var value V
		if err := strictDecode(entries[name], &value); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", bucket, name, err)
		}
		decoded[name] = value
	}
	return decoded, nil
}

// strictDecode refuses an unknown key, so that a misspelt bucket is an error
// rather than a silently empty one.
func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// rootsWithoutProbes returns every root except the probes, sorted. A projection's
// source is a root too: there is nothing to project from otherwise.
func (m *Manifest) rootsWithoutProbes() []string {
	roots := slices.Concat(m.Payloads, m.Plumbing, sortedKeys(m.Internal))
	for _, name := range sortedKeys(m.Projections) {
		roots = append(roots, m.Projections[name].From)
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}
