// Package wire is the runtime the generated protocol codecs are written
// against.
//
// The Agent Client Protocol's schema carries decoding semantics that no plain
// JSON decoder implements: 378 properties recover to a declared default rather
// than failing the message, 35 arrays drop invalid items rather than failing,
// four unions have a catch-all arm whose extra properties are its payload, and
// required properties, closed enumerations and numeric bounds are all
// unenforced by Go field types alone. Generation turns each of those into
// explicit code; this package holds the pieces that code is built from, so the
// semantics live in one reviewable place instead of being restated throughout
// every generated decoder.
//
// It is deliberately ignorant of the protocol. Nothing here names a method, a
// capability or a message type, and it imports nothing from the module's
// exported API — which is what lets the generated code, which does name all of
// those, sit in the exported package and still share one runtime.
package wire
