// Package wire is the runtime the generated protocol codecs are written
// against.
//
// The Agent Client Protocol's schema carries decoding semantics that no plain
// JSON decoder implements: selected properties recover to defaults, selected
// arrays drop invalid items, and open unions retain a catch-all arm's extra
// payload. Required properties, closed enumerations and numeric bounds are all
// unenforced by Go field types alone. Generation turns each of those into
// explicit code built from the pieces this package holds.
//
// It is deliberately ignorant of the protocol. Nothing here names a method, a
// capability or a message type, and it imports nothing from the module's
// exported API — which is what lets the generated code, which does name all of
// those, sit in the exported package and still share one runtime.
package wire
