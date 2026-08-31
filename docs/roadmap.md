# Roadmap

The order the package gets built in. Each layer is a product that works end to
end before the next one starts, per [AGENTS.md](../AGENTS.md): "Start from the
smallest version that works end to end, and add each new capability on top of a
product that already works."

This ordering was revised after
[design-review.md](./design-review.md#recommended-roadmap-changes). The change
that matters: what used to be one "wire" layer is now a spike followed by the
full generator. The schema turned out to have five union classes, 378
occurrences of `x-deserialize-default-on-error` and 35 of
`x-deserialize-skip-invalid-items`, so "generate all 265 definitions" was hiding
the possibility that the chosen Go representation cannot express the schema at
all. Finding that out on eight types is cheap; finding it out on 265 is a
rewrite.

## 1. Wire-semantics spike

Not a released layer — a question answered before the generator is written.
**It is the first slice of the real generator, not a throwaway.** An earlier
draft said "hand-write or generate", which permits a hand-written experiment that
proves an encoding idea the generator then cannot reproduce — and it plans code
whose replacement is scheduled, which is the stopgap
[AGENTS.md](../AGENTS.md) rules out. So layer 1 builds the permanent pieces on a
deliberately tiny surface:

- pin `schema.json`, `meta.json`, their provenance and upstream licence;
- write the real schema traversal and the real generator command;
- use a root manifest holding only the representative definitions — small by
  scope, not by design;
- put recovery, validation, union selection and JSON-value logic in the runtime
  package the full generator will use;
- generate the representative types and run the fixtures against them.

Layer 2 grows the manifest and the coverage. It does not replace this
architecture.

The representative set has to cover every shape that could invalidate the
model:

- one closed string enumeration and one open string union;
- one closed discriminated object union;
- one of the four open object unions, including its `not` exclusion and its
  retained extra payload;
- a required slice, which must encode as `[]` and never as `null`;
- a field carrying `x-deserialize-default-on-error`;
- an array carrying `x-deserialize-skip-invalid-items`;
- a type with real numeric or string constraints;
- an optional-and-nullable field, proving all three of omitted, `null` and
  present — there are 357 such property occurrences, and a pointer with
  `omitempty` collapses two of the three states;
- a *required* field carrying `x-deserialize-default-on-error`, which recovers
  from malformed input but must still fail when the property is absent, if that
  is what the oracle does.

**Done when** four assertions hold, because "round-trips byte-identically" is
not a property `encoding/json` has and was the wrong oracle to promise:

1. Valid input is accepted by both SDKs and normalises to semantically equivalent
   JSON. Whitespace, key order, number spelling and escapes may all differ while
   the decoded value is the same.
2. Malformed input subject to schema recovery normalises to the same value in
   both SDKs.
3. The Go encoder matches golden output for values constructed in Go.

4. `_meta` and open-union extra properties survive as equivalent JSON values —
   *not* as identical bytes. The TypeScript SDK parses `_meta` through
   `z.record(z.string(), z.unknown())` and reattaches parsed values, so its bytes
   are gone too and no cross-SDK assertion could hold.

### The oracle has to be reproducible, and a sibling checkout is not

The cross-check against the TypeScript SDK is the point of the layer — two
endpoints from one Go implementation can agree with each other and both be wrong.
But `~/Desktop/acp-typescript-sdk` is evidence for a review, not a CI dependency,
and installing `@agentclientprotocol/sdk@1.4.0` does not fix that: the package's
`exports` map reaches `dist/acp.js`, the experimental entry points and the raw
schema JSON, and **nothing else**. The generated Zod validators and the
schema-deserialisation helpers — the actual oracle — are not reachable from the
published package.

So layer 1 commits a **fixture corpus plus a pinned updater**: the updater builds
the SDK from a recorded commit and writes the expected values, CI replays the
committed fixtures with no network and no Node build, and a periodic job runs the
updater to catch upstream drift. This keeps the normal build hermetic while
keeping the oracle honest.

Committed alongside: the SDK commit, the npm package version, the schema release
tag, the Node version policy, and the single command that regenerates
everything. Without those five, "the two SDKs agree" is an anecdote rather than
release evidence.

## 2. Wire

The generator, for real.

- Vendor and pin both `schema.json` and `meta.json` at `schema-v1.21.0`.
- Generate the public payload types, the internal routing and envelope types, the
  method table, validation and codecs — with the visibility split in
  [design.md](./design.md#what-is-generated-and-what-is-exported), because
  `gorelease` will hold the module to whatever is exported at the first tag.
- Drive generation from a committed **root manifest**, not from a count written
  into prose. CI computes the `$ref` closure from the manifest and fails if the
  exported set differs, so growing the API is a diff in one file. The manifest
  names implemented payloads, the plumbing they need — `Error` and `ErrorCode`
  are reached from response envelopes rather than from any method's params, so
  they are roots once `Error` is exported — and any extension marker types.
- Copy upstream's UNSTABLE marker into the doc comment of every symbol that
  carries one — 18 of them are inside that closure and cannot be avoided.
- Commit the **complete** capability table now, classifying every one of the 43
  methods as baseline, gated by a named predicate, or not yet implemented. It
  belonged in layer 5 in the previous draft, which was too late: layer 3 already
  exchanges capabilities and reserves every standard method name, and layer 4
  needs authentication and session gating — neither can enforce an invariant
  whose classification does not exist yet. Later layers activate rows; they do
  not invent the classification then. CI compares the table against every method
  in `meta.json` even while most payload types are not yet generation roots,
  because the schema has `x-method` and `x-side` but nothing linking a method to
  a capability, so the table is hand-maintained and has to be checked rather than
  trusted.
- Regeneration and the cross-SDK fixture comparison are both CI checks.

**Done when** the committed output is reproducible, the fixtures agree with the
TypeScript SDK, and the exported set equals the computed closure.

## 3. Link

The connection machinery, with no ACP semantics in it.

- `internal/jsonrpc2`, forked from `golang.org/x/tools/internal/jsonrpc2_v2`,
  upstream copyright and licence headers intact.
- `Transport` and `Connection`, with the concurrency and shutdown contract in
  [design.md](./design.md#transports) — not just the signatures.
- The connection state machine, and request-level cancellation via
  `$/cancel_request`.
- `InMemoryTransport` first, because it is what the later layers are tested with
  and it keeps `os` out of the package.

**Done when** a client and an agent complete `initialize` over an in-memory pipe,
and shutdown and cancellation are tested with `testing/synctest` rather than
sleeps.

## 4. Turn

The first release worth using.

- `session/new`, `session/prompt`, `session/update`,
  `session/request_permission`, behind the `*ClientSession` and `*AgentSession`
  handles.
- `authenticate`. The previous revision put `ErrAuthRequired` on the ordinary
  path out of `session/new` and never implemented the method that answers it — a
  client that can recognise "authenticate first" and cannot authenticate is not
  end to end against an agent that requires it.
- The session-turn state machine: one context per turn, pending
  `session/request_permission` indexed by session, each resolved exactly once
  with the `cancelled` outcome when a turn is cancelled, and the race against a
  late user decision decided in the connection rather than the application.
- Turn cancellation as its own operation, separate from request cancellation —
  see [design.md](./design.md#two-cancellations-and-who-owns-each).
- The error model: `ErrorCode int32` with eight predefined constants and unknown
  in-range codes preserved, `(*Error).Is` comparing codes so `ErrAuthRequired`
  works as a sentinel, and remote `-32800` distinguished from local context
  completion rather than flattened into `context.Canceled`.
- `StdioTransport` and `CommandTransport`.

**Done when** this SDK interoperates with a TypeScript SDK process over stdio: a
full turn with a permission prompt in the middle, and a cancelled turn whose
final updates still arrive after the original caller has stopped waiting. Tag
`v0.1.0` only after that. Two Go endpoints talking to each other share any wire
bug they have and are not release evidence.

## 5. Workspace

- `fs/read_text_file` and `fs/write_text_file` — two independent capability
  booleans, so two independent handlers.
- The terminal group: all five methods together, because
  `ClientCapabilities.terminal` is one boolean covering all of them.
- The capability invariant enforced in both directions: construction fails if an
  advertised capability lacks its complete handler set, an inbound call to an
  unadvertised method is refused, and an outbound call the peer never advertised
  fails locally.

## 6. The rest

`session/load` and `session/resume`, modes and config options, `elicitation/*`,
the `mcp/*` passthrough, `providers/*`, `document/*`, `nes/*` — each only once
its capability-to-handler grouping is defined.

The extension boundary (`Call`, `Notify`, fallback handlers for `ExtRequest` /
`ExtNotification`) lands with layer 3, not here: it is how anyone implements an
ACP extension at all, and leaving it out would make the SDK a ceiling.

Not in this list, and deliberately:

- **Middleware.** The internal dispatch seam exists from layer 3 so it can be
  added without an API break. It stays unexposed until something needs it.
- **HTTP and WebSocket transports.** Work in progress upstream.
- **Schema v2.** Unstable upstream (`schema-v2.0.0-alpha.3`). If needed before it
  stabilises it goes in a separate package whose name says so.

## Open questions

Things that are not decided, listed so they are not mistaken for decided.

- **Where validation runs for large payloads.** Validating every inbound and
  outbound message is the correct default, but `session/update` arrives
  continuously for the whole of a turn. Whether that needs a fast path is a
  question for a benchmark, not for this document.

### Resolved

- **Session handles across a reconnect.** Connection-bound. `LoadSession` and
  `ResumeSession` on a new connection return a new handle; callers keep the
  `SessionID`. A handle that silently re-pointed at another transport would be a
  lifetime nobody can reason about.

- **v1's unstable subset.** Deferring it is not available, and the count is why.
  Of the 52 UNSTABLE definitions, 38 are unstable as types and 14 are stable
  types with unstable fields — including `SessionUpdate`, `PromptResponse`,
  `ToolCall` and both capability structs. Following every `$ref` from the
  operations layers 3–5 implement leaves 18 of the 38 type-level-unstable types
  inside the closure. So generation scope follows **reachability from implemented
  operations**, generated types are generated whole, and the module's
  compatibility promise carves out upstream-unstable symbols in writing before
  v1.0. See
  [design.md](./design.md#the-v1-schema-lane-only-which-is-not-the-same-as-stable).

  This reverses the recommendation offered before the numbers were computed.

- **Union decode helper.** No generic `As[T]` initially. Type switches and type
  assertions are standard Go and sufficient; a helper earns its place only if
  real callers show repeated logic it materially improves.
- **Ungated methods.** Refuse them, in both directions. Capabilities are an
  authority boundary, not presentation metadata. Extension methods stay on their
  own explicit fallback path.
- **Signals in `Agent.Run`.** A library does not own operating-system signals.
  `Run` stops when its context is cancelled or its transport ends; a `main`
  package owns `signal.NotifyContext`.
