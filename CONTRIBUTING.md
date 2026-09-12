# Contributing

Engineering conventions that apply to every change live in [`AGENTS.md`](./AGENTS.md), and the rules unique to
this repository in [`PROJECT_RULES.md`](./PROJECT_RULES.md). This page covers only what a contributor needs
that neither of those states: the toolchain, the upstream protocol boundary, and how wire behaviour is
evidenced.

Design decisions live in [`design/design.md`](./design/design.md). API doc comments define caller-visible
contracts. Each repository check documents the failure it prevents in `.golangci.yml`, `ci.yml`, or its own
script.

## Requirements

- **Go 1.25 or newer.** CI tests the exact language floor, so a newer local toolchain cannot hide an
  unsupported language dependency. Go 1.25 is required for `testing/synctest`, which tests concurrency and
  shutdown without sleeps.
- **Pinned checkers.** `golangci-lint` v2 (CI pins v2.13.1), `deadcode` from `golang.org/x/tools` (v0.49.0),
  `gofumpt` (v0.11.0), `shfmt` (v3.13.1), and `govulncheck` (v1.7.0). The release check pins
  `golang.org/x/exp/cmd/gorelease` at `v0.0.0-20260820142414-ca536658362e`.
- **Node.js 22.18 or newer** when changing Markdown. The exact toolchain is in `.tools/package-lock.json`.
- Tests use the standard `testing` package.

This is one module at the repository root. There is no workspace file, and a plain checkout builds.

The Markdown toolchain installs into `.tools` rather than the repository root. Go's `./...` pattern walks every
directory that does not begin with a dot or an underscore, so a root `node_modules` would expose vendored Go
source from npm dependencies to `go build`, `go vet`, and the reachability gate. The npm scripts return to the
repository root before checking Markdown.

## Run checks locally

The focused development loop while editing:

```sh
gofumpt -w .
go generate ./...
go test ./...
```

The required checks before opening a pull request:

```sh
set -eu
unformatted=$(gofumpt -l .)
test -z "$unformatted"
shfmt -d scripts
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
govulncheck ./...
scripts/check-reachability.sh
go mod tidy -diff
go run ./internal/cmd/schemagen -check
(cd .tools && npm ci && npm run docs:check)
```

CI additionally runs fuzz targets, the Go 1.25.0 floor, platform-specific lint, and the cross-compilation
matrix. Run a fuzz target locally with:

```sh
go test . -run '^$' -fuzz FuzzName -fuzztime=30s
```

`docs:check` spell-checks and lints every tracked Markdown file, and the two checkers are meant to see the same
set. `cspell.json` names `.github/**/*.md` separately because a `**` glob does not descend into a leading-dot
directory — without it the pull request template is linted and not spelled, which is how `design/design.md`
once went unchecked for months. Its word list carries only words the checked prose actually uses; a term that
lives solely in Go comments does not belong there, because no check reads them.

## The protocol is upstream

The wire grammar belongs to the [Agent Client Protocol
specification](https://github.com/agentclientprotocol/agent-client-protocol). A change to a message name,
field, or its meaning is a change to that specification and belongs in an issue there, not in a pull request
here. What belongs here is the Go shape of it: how the messages are typed, how a connection is owned, and what
a caller holds and for how long.

Say which schema version a wire-affecting change follows.

### The protocol types are generated

Do not edit `schema.gen.go` or `schema/exported.txt`. `internal/cmd/schemagen` writes both from
`schema/schema.json`, and CI regenerates and compares, so a hand edit is reverted by the next `go generate` and
reported by the gate before that.

- To widen the API, add a root to [`schema/manifest.json`](./schema/manifest.json) and regenerate. Scope is the
  transitive `$ref` closure of that file.
- To change how a shape is generated, change the generator. It refuses a construct it cannot represent instead
  of emitting code that merely compiles, so a new shape produces a generation failure that names the
  definition.
- To move the schema pin, follow [`schema/README.md`](./schema/README.md). It is a reviewable commit, never a
  build step.

## Interoperability evidence

Two Go endpoints can share the same wire bug, so wire behaviour is checked against implementations this
repository did not write. Three corpora do that, and none of them may be written by hand.

| Corpus | What it records | How it is produced |
| --- | --- | --- |
| `testdata/fixtures` | What TypeScript SDK validators make of each input | `scripts/update-fixtures.sh` |
| `testdata/interop` | This module's client against an agent on the reference SDK | `scripts/interop.sh` |
| `testdata/zed` | The agent side against a real editor | Manual, see below |

`go test` replays all three with no network and no Node.

**Fixtures.** The updater feeds independently implemented validators this repository's pinned published
schema; the SDK's normal v1 input is explicitly unstable and is not a stable-protocol oracle. Add a case by
writing its `name`, `why`, and `input`, then run the updater to fill in the outcome. It needs Node and either
the network or `ACP_TYPESCRIPT_SDK` pointing at a checkout of the pinned commit, and it refuses anything that
is not that commit.

**Interop.** `scripts/interop.sh` runs the reference agent as a real subprocess against a pinned checkout. Add
a scenario in `scripts/interop-agent.ts` and `internal/cmd/interop`, then record it with the script. Re-record
when the pin moves.

**Zed.** Recording the agent side against a real editor requires a person to drive Zed. Build an agent and put
a wrapper in front of it that tees both directions of the stream to a file:

```sh
go build -o /tmp/acp-agent ./examples/agent
cat > /tmp/acp-agent.sh <<'SH'
#!/bin/sh
exec tee -a /tmp/from-client.jsonl | /tmp/acp-agent | tee -a /tmp/to-client.jsonl
SH
chmod +x /tmp/acp-agent.sh
```

Register the wrapper in Zed's `settings.json`, open a scratch directory, and start a thread with it:

```json
{
  "agent_servers": {
    "acp-go": { "type": "custom", "command": "/tmp/acp-agent.sh", "args": [], "env": {} }
  }
}
```

Drive one conversation that includes a prompt, the permission dialog, and the stop button. Save both logs
beside the existing transcript, and record the date and the editor version from its `clientInfo` message.

This replay is a corpus rather than a gate: nothing in it can catch the editor changing. What it catches is
this package changing under bytes another implementation actually sent.

## Public API changes

Any exported change must include:

- A comment that defines behaviour and edge cases instead of restating the name.
- A test in the external package (`acp_test`) showing it from a caller's side. Tests go inside the package only
  in `*_internals_test.go`, and only for properties with no public form.
- Cancellation and concurrency semantics where they apply.
- A [CHANGELOG.md](./CHANGELOG.md) entry when existing callers must change.

A `deadcode` finding on an export asks for caller-side contract coverage. Removal additionally requires an API
review showing that the responsibility is misplaced, duplicated, or cannot be given a coherent contract.

Two changes are compatibility decisions rather than routine cleanup: adding a method to an exported interface
is breaking, and raising the `go` directive raises every dependent's toolchain floor.

## Write tests

State behaviour, not implementation: which message a cancelled request must still produce, what a half-read
stream must not decode, what an unknown method must answer. A test whose expectation is read out of the code
under test passes however that code changes, so constants and message shapes are spelled out rather than
imported.

A guard nobody has seen fail is a guard nobody knows is wired up. Where a test exists to catch a specific
mistake, make the mistake once and check that it fails.

## Create a release

Tags are immutable dependency promises; never move or recreate a published tag.

[`scripts/release.sh`](scripts/release.sh) is the only supported release path. It refuses a dirty tree, an
existing tag, a `replace` directive, and a version the repository's own prose does not claim — `CHANGELOG.md`
must have the entry and `README.md`'s table must name it, because a tag is the one statement of the version
that a later commit cannot correct. It then checks compatibility against the preceding tag before creating and
pushing the release.

Inspect the dry run before enabling its only destructive mode:

```sh
scripts/release.sh X.Y.Z
scripts/release.sh X.Y.Z --execute
```

Do not create tags or edit versions by hand; that would introduce a second release path whose ordering and
failure semantics are not guarded.

## Pull requests

Keep commits reviewable and do not mix unrelated cleanup with behavioural change. Explain the problem, the
user-visible outcome, and the trade-offs. Support every performance claim with a benchmark.
