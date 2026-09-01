// Package acp implements the Agent Client Protocol in Go.
//
// The protocol standardises the conversation between a client — a code editor or
// any other program that holds a workspace and a user — and an agent, a program
// that uses a model to read and modify that workspace. Both halves live here: a
// client drives an agent over a transport, and an agent serves a client over the
// same one, because the two are the same message grammar read from opposite ends.
//
// Messages are JSON-RPC 2.0 over a byte stream. The transport is ordinarily the
// agent subprocess's stdin and stdout, but nothing in this package requires that;
// anything that can carry newline-delimited JSON in both directions will do.
//
// # The wire types are generated
//
// The protocol's 170 stable type definitions are read from schema/schema.json,
// which is vendored from a published upstream release. The public closure is
// generated and committed as schema.gen.go, so a schema change is a reviewable
// source diff rather than a build-time download. Generated doc comments preserve
// the specification's prose.
package acp

//go:generate go run ./internal/cmd/schemagen
