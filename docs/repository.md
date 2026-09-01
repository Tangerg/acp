# The repository

How this repository is built and what each gate proves. The rules the code is
written under are in [AGENTS.md](../AGENTS.md); the workflow for changing it is in
[CONTRIBUTING.md](../CONTRIBUTING.md). This page records why the setup is shaped
the way it is.

## Where it came from

The linter set, the CI gate, the pinned tool versions and the release discipline
were ported from `~/Desktop/oolong` (`github.com/Tangerg/oolong`). That
repository had already made these decisions and argued for them in its config
comments, and re-deriving them here would only have produced a worse copy.

What carried over unchanged:

- `.golangci.yml` — roughly eighty linters against golangci-lint's default six,
  with every `settings` entry keeping the comment that says why it is set.
- The three-platform lint sweep and the cross-compile matrix.
- `floor`, `tidy`, `quality`, `fuzz` and `compatibility` jobs.
- Pinned tool versions: a linter that floats is one whose findings arrive as
  unrelated failures on somebody else's change.
- `scripts/release.sh` as the only release path, and the rule that a published
  tag is never moved.
- The issue and pull request templates, and `AGENTS.md` itself.

## Why this is one module and oolong is not

oolong is a workspace of nine modules whose root is deliberately not a module. Its
CI therefore walks `scripts/modules.sh` for every job, and runs a second pass
with `GOWORK=off` and local replacements to catch a dependency that the workspace
was quietly satisfying.

None of that applies here. This is one module at the repository root, `go build
./...` is the whole story, and carrying the workspace machinery would be
indirection with nothing behind it — the case [AGENTS.md](../AGENTS.md) rules
out. So `scripts/modules.sh` and `scripts/replace-local-modules.sh` were not
ported, and `scripts/release.sh` shrank from 511 lines of coordinated
multi-module release to the single-module form: refuse a dirty tree, an existing
tag, a missing remote or a `replace` directive, run `gorelease` against the
preceding tag, then tag and push.

## Two things the root-is-a-module change forced

Both were found by running the gate rather than by reading it, and both would
have failed on the first push.

### The Markdown toolchain lives in `.tools`

A root `node_modules` is harmless in oolong because oolong's root is not a
module. Here it is, and Go's `./...` walks every directory that does not begin
with a dot or an underscore. cspell depends on `flatted`, which ships vendored Go
source, so `go list ./...` reported:

```text
github.com/Tangerg/acp
github.com/Tangerg/acp/node_modules/flatted/golang/pkg/flatted
```

and the reachability gate reported seven unreachable functions in a dependency of
a spell checker. `go build` and `go vet` happened to pass only because that file
is valid Go.

The npm project therefore lives in `.tools`, whose leading dot Go skips, and its
scripts step back up to the repository root so cspell and markdownlint still find
their configuration and the prose. The directory is `.tools` rather than `.docs`
because `docs/` next to `.docs/` — two directories differing by one character and
meaning entirely different things — is a trap for the next reader.

### The fuzz job tolerates having no targets

`grep -rl '^func Fuzz'` exits 1 when it matches nothing, and under `set -o
pipefail` that reports "this package has no fuzz targets yet" as a failed build.
oolong never hit it because oolong has fuzz targets. The job now says so and
exits 0.

There are targets now — the codec's, asserting that normalisation is a fixed
point, which is the property schema-directed recovery can quietly break and the
one no case table finds. The guard stays: having no targets is a fact about how
far the package has grown, and it should not read as a broken build.

Related: `actions/setup-go` fails outright on a `cache-dependency-path` that
matches no file, and a module with no dependencies has no `go.sum`. The cache key
is `go.mod`.

## What each gate is for

| Job | Proves |
| --- | --- |
| `test` | Builds, vets and passes `-race` on Linux, macOS and Windows, at the language floor and the current patch release. |
| `floor` | The declared `go` directive is honest — tested with `GOTOOLCHAIN=local`, so it cannot borrow a newer toolchain. |
| `quality` | ~80 linters on three platforms, plus `deadcode` reachability and `govulncheck`. |
| `tidy` | `gofumpt`, `shfmt`, `go mod tidy -diff` and the generator are all no-ops. |
| `cross` | Vets and builds for six more `GOOS`/`GOARCH` pairs, including `wasip1` and `js`. |
| `fuzz` | Every fuzz target runs for 30s on every push, not only when someone remembers. |
| `docs` | `npm audit`, cspell and markdownlint over the prose. |
| `compatibility` | On a tag: `gorelease` against the preceding immutable tag. |

Four of these deserve a note.

**The generator belongs in `tidy` rather than in a job of its own**, because it is
the same kind of promise as `go mod tidy -diff`: a committed artifact derived from
another committed artifact, with nothing to prove beyond being current.
`go run ./internal/cmd/schemagen -check` regenerates in memory and compares, so it
also catches a generator that is not deterministic — Go map iteration is not, and
one that depended on it would pass locally and fail here.

**The race detector is the point of running the tests at all.** A connection will
read, write and dispatch from separate goroutines; the ownership rules only stay
true if something checks.

**Lint runs once per platform** because a linter only sees the files that build
for the platform it is standing on. A shadowed variable in a platform-guarded
file is invisible everywhere else, which is how one reaches `main`.

**Reachability is not a request to delete exports.** For a private function a
`deadcode` finding is ordinary dead implementation. For an exported one it means
the public contract has no caller-side test — never that the operation is
unnecessary. Whether an export belongs is an API-design question and never a
repository call count. This matters more here than in most repositories, because
a protocol SDK exists to be called from outside it.

## Layout

```text
acp/
├── go.mod                    module github.com/Tangerg/acp, Go 1.25
├── doc.go  version.go        the package, and CurrentProtocolVersion
├── opt.go  meta.go           the hand-written parts of the wire vocabulary
├── schema.gen.go             the protocol types, generated and committed
├── schema/                   the vendored schema, the generation roots, and what
│                             their closure became
├── internal/wire/            the runtime the generated codecs are written against
├── internal/cmd/schemagen/   the generator
├── testdata/fixtures/        the cross-SDK corpus, replayed by go test
├── AGENTS.md                 the rules the code is written under
├── CLAUDE.md -> AGENTS.md    one file, two names, no drift
├── docs/                     these pages
├── .tools/                   the Markdown toolchain, out of Go's way
├── scripts/                  check-reachability.sh, release.sh, update-fixtures.sh, oracle.ts
├── .github/                  ci.yml, dependabot, issue and PR templates
└── .golangci.yml  cspell.json  .markdownlint-cli2.yaml
```

`CLAUDE.md` is a symlink rather than a copy: two files with the same rules drift,
and the one that drifts is whichever was not edited.

## The language floor is 1.25, not the newest release

oolong declares Go 1.27 because it uses Go 1.27 method syntax. This module
inherited that number and had no such reason, which
[design-review.md](./design-review.md#advisory-recommendations) caught: a
library's floor is a cost it imposes on everyone who imports it, and it should be
the lowest one it can justify.

1.25 is justified — `testing/synctest` is stable there, and
[roadmap.md](./roadmap.md#3-link) commits to testing the connection layer's
shutdown and cancellation with it rather than with sleeps. Nothing in the design
needs anything newer; the official MCP Go SDK declares 1.25 for comparable code.

The gofumpt pin moved with it. It was a pseudo-version because that was the first
revision parsing Go 1.27 methods; with no 1.27 syntax to parse, the released
v0.11.0 formats this tree identically and is what CI installs.

## Versioning

Pre-1.0, and the module version is not the protocol version.
`acp.CurrentProtocolVersion` is `1` because that is what ACP's `initialize`
negotiates; a module release never moves it, and a protocol release moves it only
when the wire grammar changes incompatibly.

It is `CurrentProtocolVersion` rather than `ProtocolVersion` because the schema
defines a `ProtocolVersion` type and the schema's names are not this repository's
to reassign — see
[design.md](./design.md#names-are-the-schemas-put-through-gos-initialism-rule).

The vendored schema is pinned to an upstream release tag — `schema-v1.21.0` — and
moving it is a deliberate, reviewable commit, not a build-time download. See
[design.md](./design.md#the-wire-types-are-generated).
