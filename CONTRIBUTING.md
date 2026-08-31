# Contributing

Focused issues and pull requests are welcome. Before changing behaviour or an
exported API, read the rules this repository is built under in
[AGENTS.md](./AGENTS.md).

## Requirements

- Go 1.27 or newer. The module declares and independently tests that language
  floor, so a local toolchain cannot hide a newer language dependency from a
  downstream user.
- `golangci-lint` v2 (CI pins v2.13.1), `deadcode` from `golang.org/x/tools`
  (v0.49.0), `gofumpt` (`v0.11.1-0.20260820074422-a2bc6805583d`), `shfmt`
  (v3.13.1), and `govulncheck` (v1.7.0). The release check pins
  `golang.org/x/exp/cmd/gorelease` at `v0.0.0-20260820142414-ca536658362e`. The
  gofumpt pseudo-version is the first upstream revision that understands Go 1.27
  methods with type parameters; keep it exact until that support has a tagged
  release.
- Node.js 22.18 or newer when changing Markdown. The exact toolchain is in
  `.docs/package-lock.json`.
- Tests written with the standard `testing` package.

This is one module at the repository root. There is no workspace file, and a plain
checkout builds.

The Markdown toolchain installs into `.docs` rather than the repository root. Go's
`./...` walks every directory that does not begin with a dot or an underscore, so a
root `node_modules` would hand an npm dependency's vendored Go source to `go build`,
`go vet` and the reachability gate — cspell ships one today. The npm scripts step
back up to the root, so the checkers still see the configuration and the prose where
those live.

## Development workflow

While iterating:

```sh
gofumpt -w .
go test ./...
```

Before opening a pull request, the whole gate CI runs:

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
(cd .docs && npm ci && npm run docs:check)
```

Fuzz targets run in CI on every push. Run one locally with:

```sh
go test . -run '^$' -fuzz FuzzName -fuzztime=30s
```

## The protocol is upstream

The wire grammar belongs to the [Agent Client Protocol
specification](https://github.com/agentclientprotocol/agent-client-protocol). A
change to a message name, field, or its meaning is a change to that specification
and belongs in an issue there, not in a pull request here. What belongs here is the
Go shape of it: how the messages are typed, how a connection is owned, what a caller
holds and for how long.

Where this implementation and the published schema disagree, the schema is right and
the disagreement is a bug. Say which schema version a wire-affecting change follows.

## Public API changes

Any exported change must include:

- A comment that defines behaviour and edge cases, not just restates the name.
- A test in the external package (`acp_test`) showing it from a caller's side.
  Tests go inside the package only in `*_internals_test.go`, and only for
  properties with no public form.
- Cancellation and concurrency semantics where they apply.
- A [CHANGELOG.md](./CHANGELOG.md) entry when existing callers must change.

Related construction settings belong in a `Config` struct, whose optional fields
have useful zero meanings. Do not add a functional-options API.

Repository usage is not an API-retention criterion. A library operation may exist
solely for downstream callers or to satisfy a consumer-owned interface. A `deadcode`
finding on an export asks for caller-side contract coverage; removal additionally
requires an API review showing that the responsibility is misplaced, duplicated, or
cannot be given a coherent contract.

Adding a method to an exported interface is breaking. Raising the `go` directive
raises every dependent's toolchain floor. Both are compatibility decisions rather
than routine cleanup.

## Releases

Tags are immutable dependency promises; never move or recreate one that has been
published. [`scripts/release.sh`](scripts/release.sh) is the only supported release
path. It refuses a dirty tree, an existing tag and a `replace` directive, runs the
pinned compatibility policy against the preceding tag, and then creates and pushes
one.

Inspect the dry run before enabling its only destructive mode:

```sh
scripts/release.sh X.Y.Z
scripts/release.sh X.Y.Z --execute
```

Do not create tags or edit versions by hand; that would introduce a second release
path whose ordering and failure semantics are not guarded.

## Tests

State behaviour, not implementation: which message a cancelled request must still
produce, what a half-read stream must not decode, what an unknown method must
answer. A test whose expectation is read out of the code under test passes however
that code changes, so constants and message shapes are spelled out rather than
imported.

A guard nobody has seen fail is a guard nobody knows is wired up. Where a test
exists to catch a specific mistake, make the mistake once and check that it fails.

## Pull requests

Keep commits reviewable and do not mix unrelated cleanup with behavioural change.
Explain the problem and the user-visible outcome, the trade-offs, and — for any
performance claim — the benchmark that supports it.
