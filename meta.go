package acp

// Meta is the value of the _meta property the protocol reserves so that clients
// and agents can attach metadata to any interaction. Implementations must not
// make assumptions about the values at these keys.
//
// The values are decoded as ordinary JSON values, which is what the reference
// implementation does — it parses _meta through a record of unknown and
// reattaches the parsed values, so neither SDK keeps the original bytes. What
// survives is an equivalent JSON value, not an identical encoding.
//
// See the protocol's extensibility documentation:
// https://agentclientprotocol.com/protocol/extensibility
type Meta map[string]any
