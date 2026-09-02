package acp

// CurrentProtocolVersion is the Agent Client Protocol major version this package
// speaks.
//
// The value identifies a wire grammar, not a module release. A connection rejects
// any different version because this package cannot safely infer compatibility.
//
// It is named Current because the schema already gives the type a version travels
// as the name ProtocolVersion, and the schema's names are not this package's to
// reassign.
const CurrentProtocolVersion ProtocolVersion = 1
