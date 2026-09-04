# Contributing

The project is a subproject of Apache SkyWalking. Discussion happens on the
[dev mailing list](mailto:dev@skywalking.apache.org) and in
[GitHub issues](https://github.com/apache/skywalking-ai-sessionizer/issues). Changes arrive as
pull requests against `main`.

## Build and test

Go 1.27 or later. The module has one dependency, a YAML parser, and adding another needs a reason.

```sh
make build          # -> ./bin/asz
make test           # the whole suite, with the race detector
make test-e2e       # the scenarios, the chain tests and the boundary rules, verbose
make scenarios      # every scenario through the built command, as CI runs them
make check          # vet, lint, license headers, dependency licenses, tests: what CI runs
make help           # every target
```

The end-to-end tests are scenarios under `tests/scenarios/`: a YAML description of a conversation
and an expectation file beside it, run in both formats by `go test ./tests/` and, through the
command, by `make scenarios`. See [Scenarios](scenario.md). `tests/chain/` holds what a scenario
cannot express, and `tests/boundary/` the import rules between the collector side and the server
side.

## Before opening a pull request

- `make check` passes. CI runs the same targets and one required check fans them in.
- Every source file carries the Apache license header. `make license-fix` inserts missing ones.
- Comments, docs and commit messages are plain English. Someone reading this project is often not
  a native English speaker, and the code is the hard part. No abbreviations, no slang, no figurative
  words where a common one works. Short sentences, one idea each.
- A comment says why, not what. The code already says what it does. A comment earns its place by
  recording the reason, the measurement, or the failure that forced the choice.
- Commit messages carry no AI attribution. The person who commits is the author, and this is an
  Apache project whose history records people.

## Evidence

Every claim in the documentation is backed by a measurement against a real corpus. When changing
the docs, keep that standard: state what was measured and on what sample, and say `unavailable`
rather than approximating. The same rule holds in the code. The model never invents missing input,
sessions, parentage or causal links; a reference that cannot be resolved is carried as data and
shown as such.

`design-notes/` holds the working notes: measurements, corrections and open questions, unpolished
by intent. `docs/` is the official documentation and is published on the SkyWalking website.

## Invariants

The project notes in `CLAUDE.md` at the repository root list the invariants worth knowing before
changing assembly or collector code: rounds are immutable and carry no wall-clock time, ids come
from evidence and never from position, absence means unchanged and never deleted, landed files are
write-once, the index is derived and disposable, and the model's vocabulary must stay free of any
runtime's field names. Read them first.

## Two sides

The collector side, under `internal/adapters`, lands Session Data. The server side, `assemble`,
`parse`, `view`, `verify` and `sessionflow`, assembles and serves it. They meet only at the
storage root and never import each other; the test in `tests/boundary` fails when one does. Keep
it that way. A later split into a collector binary and a server binary depends on it.

## Adding an adapter

An adapter lives under `internal/adapters/<runtime>/` and produces
[Session Data](../formats/session-data.md). It declares a glossary that maps every name the model
can emit to the runtime's own word for it, including entries that say the runtime has no word, and
a test fails when a name has no entry. The [Claude Code adapter](../adapters/claude-code.md) is the
worked example, and its documentation page shows the standard: every mapping stated with its
evidence, and every concept the runtime cannot supply said to be unavailable.
