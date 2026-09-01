// Package jsonrpc2 is the JSON-RPC 2.0 message layer.
//
// # Where it came from
//
// messages.go and wire.go are forked from
// golang.org/x/tools/internal/jsonrpc2_v2 at v0.49.0, with the Go Authors'
// copyright and BSD licence intact — LICENSE in this directory is theirs. The Go
// team has run this code under gopls for years, and request identifiers, the
// envelope and the result-or-error discrimination are exactly the parts a
// hand-written implementation gets wrong first.
//
// # What was left behind, and why
//
// Upstream's connection machinery — conn.go, frame.go, net.go, serve.go — is
// not forked. Its abstractions are a byte-stream framer, a dialer, a server, a
// binder and a preempter, and this module's Transport interface already stands
// where the first of those would: a transport hands over whole messages, so
// framing is the transport author's business and not this package's. Forking the
// rest would have meant carrying a dialer, an idle timeout and a preemption hook
// that nothing here uses, which is the speculative abstraction AGENTS.md rules
// out. The connection is written against these message types instead.
//
// Four changes were also made around the forked files:
//
//   - EncodeIndent. Nothing in a protocol stream wants indented JSON.
//   - Upstream's non-standard error sentinels. It uses -32000 for "overloaded"
//     and -32002 for "server is closing"; the Agent Client Protocol defines
//     -32000 as authentication required and -32002 as resource not found. Two
//     meanings for one code in one binary is a bug waiting to be found by
//     somebody debugging a live connection, so the protocol's meanings are the
//     only ones this module has. What remains is the two codes the decoder
//     itself needs.
//   - Request IDs retain explicit null and reject non-integral or out-of-range
//     numbers. The published ACP schema narrows the numeric arm to int64, while
//     upstream's generic JSON-RPC representation uses float64 and collapses null
//     into an absent identifier.
//   - Inbound envelopes decode through a raw-member map. Looking up jsonrpc is
//     exact where decoding a tagged struct would also accept JSONRPC, and the map
//     preserves presence separately from zero values. Responses and error objects
//     therefore enforce their required, mutually exclusive members without a
//     second representation of the same envelope.
package jsonrpc2
