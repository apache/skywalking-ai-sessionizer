# Command Line

`asz` is one binary with one subcommand per step. Every command reads `asz.yaml` from the working
directory unless `-config FILE` names another file. Commands that take a `SESSION` accept a Claude
Code session id; a conversation `ID` is the same value, because the adapter maps one session to
one conversation.

```text
asz sources [-config FILE]           list discovered sessions and their sources
asz collect [-config FILE] [-once]   land new source data into the storage root
asz index [-config FILE] [SESSION]   report what the derived index holds
asz show [-config FILE] SESSION ID   resolve a record id or tool-use id to its payload
asz parse [-config FILE] [SESSION]   assemble conversation structure into a round chain
asz repack [-config FILE] DEST [SESSION]  re-cut landed files into DEST under the configured budget and build its chains
asz conversation [-config FILE] ID   fold a conversation's rounds and show the structure
asz view [-config FILE] [ADDR]       serve the conversations as a page
asz push [-config FILE] [-once]      send landed files and rounds to an OpenTelemetry logs receiver
asz glossary                         what the runtime calls the things the model names
asz verify [-config FILE] [SESSION]  check landed data and round chains are intact
asz version                          print the version
```

## Flags

| Flag | Meaning |
| --- | --- |
| `-config FILE` | Configuration file. Default: `./asz.yaml` when present, else the compiled defaults. |
| `-once` | Single pass, then exit. Overrides `collector.mode`. |
| `-terms MODE` | Name things in the model's words (`unified`, the default), the runtime's (`native`), or `both`. |

## sources

Discovers sessions under the source root and counts their files without landing anything. One
row per session: directories, execution streams, child-agent sidecars, workflow journals and
manifests. Sessions removed by the include and exclude filters are counted on the `filtered` line.

## collect

Lands what is new from every source into the storage root and updates the index. In watch mode it
repeats every `interval`; with `-once` or `mode: once` it makes one pass and exits. One line per
pass:

```text
[17:14:09] sessions=44 sources=5867 landed=5867 records=359292 bytes=1.0GB indexed=359292 gone=0 conflicts=0 busy=0 pending=0 errors=0 (2m39.473s)
```

| Field | Meaning |
| --- | --- |
| `sources` | source files seen this pass |
| `landed` | sources that produced new data |
| `records` | source records landed |
| `indexed` | index entries across every session touched |
| `gone` | a source deleted since the last pass. Normal: Claude Code prunes transcripts, and the landed copy outlives them. |
| `conflicts` | a source was rotated, truncated or rewritten behind its cursor. Collection stopped for it. |
| `busy` | a session skipped because another collector holds its lock |
| `pending` | a source still had data when the per-pass limit was reached |
| `errors` | listed on standard error after the line |

`pending` or `errors` above zero means the pass did not collect everything, and the command exits
non-zero. A clean pass is `pending=0 errors=0`.

## index

Reports the derived index of one session, or of every session: entries, joinable content blocks,
streams and interned strings, plus a count of records by kind. The index holds identifiers and
roles only, never text, and can be deleted; the next collect or parse rebuilds it from the landed
files.

## show

Resolves one record id, or one tool-use id, to the landed record that carries it and prints the
record. This is the way to see exactly what a step in a conversation is backed by.

## parse

Assembles every session, or one, and appends a round to each conversation's chain when something
changed. One row per session:

```text
SESSION                               ROUND  SEQ   NODES  RELS  UNRES  TALKS  RUNS  STEPS  TOOLS        CHILDREN
0438c73b-2367-4ed5-9de3-13ef9a17ed01  1      305   17249  703   0      357    445   11473  5620/5620    131/131
```

`ROUND` is the round written, or `-` when nothing changed. `SEQ` is the landed sequence the
conversation now covers. `UNRES` counts references that could not be resolved. `TOOLS` and
`CHILDREN` are joined out of total. The command exits non-zero when any session failed to parse,
so a loop or a CI step can read the status instead of the log.

## repack

Re-cuts every landed file of the storage root, or of one session, into `DEST` so that no file
exceeds the configured `max_delta_bytes`, then assembles `DEST`'s chains. Every record keeps its
bytes and its order; the cursors come along so collection can continue into `DEST`; the index and
the chains are rebuilt there, because a round addresses records by file and line and the old
references name positions that no longer exist. `DEST` must be a different directory: a landed file
is never rewritten in place. One row per session:

```text
SESSION                               FILES IN  FILES OUT  RECORDS  BYTES
0438c73b-2367-4ed5-9de3-13ef9a17ed01  305       312        21119    42.1MB
```

This is how an existing root is brought under a new budget after `max_delta_bytes` changes, and
it needs no source files.

## conversation

Folds a conversation's chain and prints the structure: rounds and head digest, the landed range,
entity counts, and a count of nodes by kind.

```text
conversation 0438c73b-2367-4ed5-9de3-13ef9a17ed01
  rounds     1, head 9a29d5acdd73…
  landed     through seq 305
  entities   17249 nodes, 703 relations, 0 unresolved (0 still open)
```

## view

Serves the conversations as a page on `ADDR`, `127.0.0.1:8787` by default. The list is at `/` and
one conversation at `/c/{id}`. The page reads the folded chain on demand and caches nothing beyond
the process.

With the `claude-code-local` adapter, when its source directory exists on the machine, the same
process also runs the collector and the parser: once with `-once`, or on the collector interval
otherwise. `/api/status` reports the mode, the source, the last and the next refresh and the counts
of the last pass, and the list page shows the same. On a storage root copied from another machine
there is no source, so the page serves what is there and shows no refresh.

## scenario

```text
asz scenario build FILE --format {claude-code|sd} --out DIR [--at TIME] [--scale FACTOR] [--repeat N] [--through CHECKPOINT]
asz scenario check FILE [--format {claude-code|sd|all}] [--out DIR] [--at TIME] [--scale FACTOR]
```

`build` turns a scenario file into the input a session leaves behind, under `DIR`, with a
`DIR/asz.yaml` for the ordinary commands to collect and parse it. `check` runs the scenario as a
test against its expectation file, in every format, at every checkpoint. See
[Scenarios](../guides/scenario.md).

## push

Sends every landed file and every round not yet sent to the OpenTelemetry logs receiver at
`export.otlp.endpoint`, one log record per file, then exits with `-once` or repeats every
`export.otlp.interval`. One line per pass:

```text
[10:12:03] files=306 bytes=47.9MB requests=5 errors=0 (1.1s)
```

A pass with errors exits non-zero with `-once`; the files whose requests failed are not recorded
as sent and go again on the next pass. See [Export over OpenTelemetry](export-otlp.md).

## glossary

Prints every name the model uses, what the runtime calls it, where in the runtime's records it is
found, and a note. A `—` in the runtime column means the model derives the concept and the runtime
has no word for it. Every command that prints names accepts `-terms native` to use the runtime's
words instead.

## verify

Checks every landed file against its digest and every round against its commit digest, for one
session or all of them. It reads the storage root only, so it works without the source files and
without a collector.

```text
checked 44 session(s), 5867 stream(s), 359292 records
checked 44 conversation chain(s), 44 round(s)
all landed data is contiguous and matches its digests
```

## version

Prints the version the binary was built with, the Go version, and the platform. A release build
says its version; a plain `go build` says `dev`.
