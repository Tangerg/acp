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
// The protocol's 265 type definitions are generated from schema/schema.json,
// which is vendored and pinned to an upstream release, and the output is
// committed as schema.gen.go. Their doc comments are the specification's own
// prose. Where one repeats upstream's **UNSTABLE** marker, that symbol is exempt
// from this module's compatibility guarantee.
package acp

//go:generate go run ./internal/cmd/schemagen
