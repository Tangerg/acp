package acp

// CurrentProtocolVersion is the Agent Client Protocol major version this package
// speaks.
//
// It is the value a client sends in the initialize request and the value an agent
// answers with when it can speak that version. The number is the protocol's, not
// this module's: a module release never moves it, and a protocol release moves it
// only when the wire grammar changes incompatibly.
//
// A peer that answers initialize with a lower number is asking to be spoken to in
// that older version. A peer that answers with a higher one is reporting a version
// this package does not implement, and the connection cannot proceed.
//
// It is named Current rather than simply ProtocolVersion because the schema calls
// the type a version travels as [ProtocolVersion], and the schema's names are not
// this package's to reassign.
const CurrentProtocolVersion ProtocolVersion = 1
