# Roadmap

The order the package gets built in. Each layer is a product that works end to
end before the next one starts, per [AGENTS.md](../AGENTS.md): "Start from the
smallest version that works end to end, and add each new capability on top of a
product that already works."

This ordering was revised after
[design-review.md](./design-review.md#recommended-roadmap-changes). The change
that matters: what used to be one "wire" layer is now a spike followed by the
full generator. The published schema has several union classes, 249 occurrences
of `x-deserialize-default-on-error` and 27 of
`x-deserialize-skip-invalid-items`, so "generate all 170 definitions" was hiding
the possibility that the chosen Go representation cannot express the schema at
all. Finding that out on eight types is cheap; finding it out on 170 is a
rewrite.

## 1. Wire-semantics spike — done

**Delivered.** The four assertions below hold, and what the layer actually turned
up is recorded under "What the spike found".

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
  present — there are 222 such property occurrences, and a pointer with
  `omitempty` collapses two of the three states;
- a *required* field carrying `x-deserialize-default-on-error`, which recovers
  from malformed input but must still fail when the property is absent, if that
  is what the oracle does.

Every one of those is covered by the manifest's six roots and the 30-definition
closure they pull in. `NewSessionRequest` and `PromptRequest` are payload roots and
carry most of the list between them; the four probes cover what no payload root
reaches yet, and [schema/manifest.json](../schema/manifest.json) records which
shape each is there to prove.

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
the SDK's independently implemented deserialisation machinery from a recorded
commit, regenerates its validators from this module's pinned published schema,
and writes the expected values. It cannot use the SDK's checked-in v1 validators:
those consume `schema.unstable.json` and admit experimental wire shapes. CI
replays the committed fixtures with no network and no Node build, and a periodic
job runs the updater to catch upstream drift.

Committed alongside: the SDK commit, the npm package version, the schema release
tag and asset hashes, the Node version policy, and the single command that
regenerates everything. Without those pins, "the two SDKs agree" is an anecdote
rather than release evidence.

They are all in `scripts/update-fixtures.sh`, which refuses the wrong SDK commit,
package version, or schema asset and generates in a clean temporary archive so a
reference checkout is never modified. `scripts/oracle.ts` runs there and writes
each fixture's outcome; `go test` replays the answers.

### What the spike found

The layer existed to find out whether the chosen Go representation can express the
schema at all. It can, and it changed four things while proving it. Each is
written up in [design.md](./design.md); this is the index.

- **The discriminant belongs to the union, not to the arm.** `ContentChunk` is the
  payload of three `SessionUpdate` arms and `ToolCallUpdate` is both an arm and an
  ordinary property elsewhere, so no payload type can own its tag. The union's
  codec splices it in, and arm naming follows from the same counting. See
  [The discriminant belongs to the union](./design.md#the-discriminant-belongs-to-the-union-not-to-the-arm).
- **Recovery has three fallbacks, and the schema states none of them.** The
  declared default, an empty array for an array that cannot be null, or nothing.
  Read off the reference implementation, because the promise is about behaviour.
  See [What recovery recovers to](./design.md#what-recovery-recovers-to).
- **Validation is not a separate pass.** It is inside `MarshalJSON` and
  `UnmarshalJSON`, which is what makes "every outbound message is validated" true
  rather than aspirational, and gets a caller who never touches the connection
  layer checked too. See
  [Validation is part of the codec](./design.md#validation-is-part-of-the-codec-not-an-afterthought).
- **`Opt` is needed less often than 222 occurrences suggests.** A property with a
  declared default has one state, not three, and an optional array has two that a
  slice already spells. See
  [Omitted, null and present](./design.md#omitted-null-and-present-are-three-states-not-two).

Two things the corpus caught that a Go-only test would not have: a `null`
discriminant was being read as the empty-string tag, because `json.Unmarshal`
accepts `null` into a string and reports nothing; and the same trap applies to
every non-nullable property, which is why decoding one goes through a check that
refuses `null` explicitly.

### What layer 1 hands to layer 2

The generator refuses what it does not implement rather than emitting something
that merely compiles, so its refusals are layer 2's work list rather than a
surprise. The manifest's 30-definition closure does not reach:

- an arm needing a generated wrapper type — `SessionUpdate`'s three chunk arms, and
  the arms with no payload at all;
- a declared default that is not a scalar — `ClientCapabilities.fs` and
  `AgentCapabilities`;
- an object that is also a union, which is how the schema flattens a union into
  `ElicitationFormMode`, `CreateElicitationResponse` and two others;
- the primitive and value unions — `RequestId`, `ElicitationContentValue`,
  `SessionConfigSelectOptions`;
- an inline object type, and a map;
- a numeric bound the Go type chosen from the `format` does not already enforce.

Each of those stops generation with a message naming the definition, so growing
the manifest is a loop rather than an audit.

## 2. Wire — done

**Delivered.** The current manifest reaches 129 of the published schema's 170
definitions. JSON-RPC envelopes are excluded by decision, and the remaining
definitions enter the closure only with an implemented public operation. What
the layer turned up is under "What layer 2 found".

The generator, for real. Three of the bullets below are what layer 1 built on a
deliberately tiny surface, and are struck through rather than deleted: what layer 2
adds is coverage, and reading the list is how the difference stays visible.

- ~~Vendor and pin both `schema.json` and `meta.json` at `schema-v1.21.0`.~~ Done.
- Generate the public payload types, the internal routing and envelope types, the
  method table, validation and codecs — with the visibility split in
  [design.md](./design.md#what-is-generated-and-what-is-exported), because
  `gorelease` will hold the module to whatever is exported at the first tag. The
  payload types, validation and codecs exist for the manifest's closure; the
  routing and envelope types and the method table are new here, and so is every
  shape in
  ["What layer 1 hands to layer 2"](#what-layer-1-hands-to-layer-2).
- ~~Drive generation from a committed **root manifest**, not from a count written
  into prose.~~ Done, including both gates: the generator's `-check` mode and the
  test that holds the package's exports against `schema/exported.txt`. What
  remains is growing the roots, and emptying the `probes` list as the payload
  roots come to reach the same shapes.
- Commit the **complete** capability table now, classifying every one of the 25
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
- ~~Regeneration and the cross-SDK fixture comparison are both CI checks.~~ Done.

**Done when** the committed output is reproducible, the fixtures agree with the
TypeScript SDK, and the exported set equals the computed closure — for every
operation layers 3 to 5 implement, rather than for the spike's representative set.

### What layer 2 found

- **The published stable schema has 25 methods.** The local TypeScript checkout
  contains experimental additions under the same v1 tag; release assets, not
  checkout-local additions, define the wire contract.
- **This SDK and the reference implementation disagree about integers.** The
  reference implementation validates an `int64` or `int32` property as a plain
  number, so most of its integer properties accept `1.5`; the schema types them
  `integer`. The schema wins, and the corpus records the disagreement as a
  divergence rather than hiding it. See
  [design.md](./design.md#what-the-round-trip-tests-actually-promise).
- **The envelopes should not be generated at all**, not merely kept unexported:
  `internal/jsonrpc2` owns JSON-RPC's grammar, and a second set of types for it
  would be two sources of truth. See
  [design.md](./design.md#the-method-table-is-generated-the-envelope-is-not).
- **Two union shapes the arm model had to bend for** — value unions, whose arms
  are different JSON shapes, and flattened unions, which are an object and a union
  at once. See
  [design.md](./design.md#two-union-shapes-the-arm-model-has-to-bend-for).
- **Not every generated type can be exported.** A published type whose field a
  caller cannot name is worse than no type, so the manifest's internal list is
  checked closed under what reaches it. See
  [design.md](./design.md#some-generated-types-are-not-exported).

One bug the corpus caught that a Go-only test would not have: a union of two
differently-typed arrays was being decoded as if the interface could decode into
itself, so every `select` config option was silently dropped from a
`session/update`. Value unions were not marked as needing their generated
selector.

## 3. Link — done

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
sleeps. All three hold.

### What layer 3 found

- **Only the message layer was worth forking.** Upstream's connection machinery is
  a framer, a dialer, a server, a binder and a preempter; this module's
  `Transport` already stands where the framer would, and the rest is a speculative
  abstraction nothing here uses. See
  [design.md](./design.md#json-rpc).
- **Inbound messages need an ordering contract, and the design had none.** A
  notification is a stream and a response must not overtake one; an inbound
  request must not be blocked by either. Getting this wrong showed up
  immediately as a turn whose last chunk arrived after the turn ended. See
  [design.md](./design.md#what-order-inbound-messages-are-served-in).
- **An `Initialize` handler would have been two sources of truth.** Everything the
  response carries is already in the config, so the connection answers it — which
  is also the one place a second initialize can be refused.

## 4. Turn — done

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

**All of it holds, including the evidence.** `scripts/interop.sh` runs this
module's client against an agent built on the reference SDK — a real subprocess,
speaking newline-delimited JSON — through four scenarios: a full turn with a
permission prompt, a cancelled turn whose final updates still arrive after the
cancellation, authentication as control flow, and every capability-gated workspace
method. It commits what crossed the wire, and `interop_test.go` replays those
transcripts with no network and no Node.

The replay is the same arrangement as the fixture corpus, for the same reason: an
oracle that runs on every build is a network dependency and a Node toolchain in a
Go module's CI. The recorded bytes are the reference implementation's, so replaying
them still checks this package against another implementation rather than against
itself — and the transcript records what the client made of the exchange as well as
the exchange, so a client that read the right bytes and drew the wrong conclusion
fails too.

What replaying gives up is precise and worth stating: it cannot notice the
reference implementation changing. Re-recording is what notices that, which is why
the updater is pinned to a commit and meant to be run on the schedule that catches
drift.

## 5. Workspace — done

- `fs/read_text_file` and `fs/write_text_file` — two independent capability
  booleans, so two independent handlers.
- The terminal group: all five methods together, because
  `ClientCapabilities.terminal` is one boolean covering all of them.
- The capability invariant enforced in both directions: construction fails if an
  advertised capability lacks its complete handler set, an inbound call to an
  unadvertised method is refused, and an outbound call the peer never advertised
  fails locally.

All three hold, and the outbound half is refused locally rather than sent: the
peer's answer would be the same, and asking wastes a round trip while making a
developer read a wire trace to find out what they forgot.

## 6. The rest — done, except elicitation

`session/resume`, `session/list`, `session/delete`, `session/close`,
`session/set_config_option` and `logout` are served. Each is gated on the
capability the schema names for it and advertised by setting its handler, so an
agent says what it can do by being able to do it.

`session/close` is the only one of them the schema puts an obligation on: the
agent "**must** cancel any ongoing work related to the session (treat it as if
`session/cancel` was called) and then free up any resources". The connection keeps
that, not the application — the outstanding prompt still owes the client the
cancelled stop reason — and it is also the one point at which a session handle can
be reclaimed, which is what stops the population being one-way.

`elicitation/create` and `elicitation/complete` remain. They are a layer rather
than an addition: the request carries a mode (form or URL) and a scope (session or
request) as two flattened unions, a request-scoped elicitation is tied to a
JSON-RPC request before any session exists, URL mode answers asynchronously
through a second correlation identifier, and form mode hands the client a JSON
Schema to render — which raises whether validating the answer belongs in this
module at all.

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

- **Which v1 source is authoritative.** The `schema.json` and `meta.json` assets
  attached to the upstream release define the stable contract. Experimental
  additions in an SDK checkout stay outside the generated surface until a
  published release includes them. See
  [design.md](./design.md#the-published-stable-schema-is-the-authority).

- **Union decode helper.** No generic `As[T]` initially. Type switches and type
  assertions are standard Go and sufficient; a helper earns its place only if
  real callers show repeated logic it materially improves.
- **Ungated methods.** Refuse them, in both directions. Capabilities are an
  authority boundary, not presentation metadata. Extension methods stay on their
  own explicit fallback path.
- **Signals in `Agent.Run`.** A library does not own operating-system signals.
  `Run` stops when its context is cancelled or its transport ends; a `main`
  package owns `signal.NotifyContext`.
