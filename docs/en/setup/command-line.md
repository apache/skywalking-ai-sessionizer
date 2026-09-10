# Command Line

`asz` is one binary with one subcommand per step. Every command reads `asz.yaml` from the working
directory unless `-config FILE` names another file. Commands that take a `SESSION` accept a Claude
Code session id; a conversation `ID` is the same value, because the adapter maps one session to
one conversation.

```text
asz sources [-config FILE]           list discovered sessions and their sources
asz collect [-config FILE] [-once]   the pipeline: land, parse, and send what the export block asks for
asz index [-config FILE] [SESSION]   report what the derived index holds
asz show [-config FILE] SESSION ID   resolve a record id or tool-use id to its payload
asz parse [-config FILE] [SESSION]   assemble conversation structure into a round chain
asz repack [-config FILE] DEST [SESSION]  re-cut landed files into DEST under the configured budget and build its chains
asz conversation [-config FILE] [-json|-yaml] ID   fold a conversation's rounds and show the structure, or print the asz.view document
asz server [-config FILE] [ADDR]     collect and serve in one process
asz view [-config FILE] [ADDR]       serve an existing storage root as a page; reads only
asz push [-config FILE]              send a storage root to an OpenTelemetry logs receiver, in one pass
asz glossary                         what the runtime calls the things the model names
asz verify [-config FILE] [SESSION]  check landed data and round chains are intact
asz version                          print the version
```

## Flags

| Flag | Meaning |
| --- | --- |
| `-config FILE` | Configuration file. Default: `./asz.yaml` when present, else the compiled defaults. |
| `-once` | One pipeline pass rather than the collector's interval. Overrides `collector.mode`. `collect` then exits; `server` goes on serving what that pass produced, because a server that exits serves nothing. |
| `-terms MODE` | Name things in the model's words (`unified`, the default), the runtime's (`native`), or `both`. |

## sources

Discovers sessions under the source root and counts their files without landing anything. One
row per session: directories, execution streams, child-agent sidecars, workflow journals and
manifests. Sessions removed by the include and exclude filters are counted on the `filtered` line.
With the `claude-code-changes` adapter enabled, a second table lists the sessions the plugin has
written change records for, with their streams and workspace.

## collect

The pipeline. One pass does three things, in this order:

1. **land** what is new from every enabled local source into the storage root, and update the index
2. **parse** every session that moved, writing the rounds
3. **send** what the export block asks for, when `export.otlp.endpoint` is set

The order matters. The push runs last so a pass sends the rounds it has just written, rather than
leaving them for the next one.

In watch mode, the default, it repeats every `interval`; with `-once` or `mode: once` it makes one
pass and exits. Every enabled local adapter is read in the same pass, so one watching source never
keeps another from running. When the `claude-code-otlp` adapter is enabled, its receiver listens
beside the pipeline for as long as the process runs.

One line per pass, and a quiet pass prints nothing:

```text
[10:12:03] refreshed: sessions=44 landed=12 records=8130 rounds=3 pushed=15 metrics=2 (2.4s)
```

| Field | Meaning |
| --- | --- |
| `sessions` | sessions seen this pass |
| `landed` | source files that produced new data |
| `records` | source records landed |
| `rounds` | rounds written by the parse step |
| `pushed` | files and rounds sent, absent when no endpoint is named |
| `metrics` | spooled metrics requests sent |
| `errors` | listed on standard error after the line |

Anything the pass could not do is listed on standard error under the line, and the next pass tries
it again: a source that was busy, a session that failed to parse, a request the receiver refused.

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

With `-json` or `-yaml` it prints the conversation's [asz.view](../formats/asz-view.md) document
instead, the same document `asz view` serves at `/api/c/{id}/view`, for a pipeline or a diff. The
YAML is a rendering of the JSON with the same keys in the same order, not a second format.

```sh
asz conversation -json 0438c73b-2367-4ed5-9de3-13ef9a17ed01 > conversation.json
asz conversation -yaml 0438c73b-2367-4ed5-9de3-13ef9a17ed01 | head
```

## server

The pipeline and the page in one process: it lands, parses and sends on the collector interval,
and serves the result on `ADDR`, `127.0.0.1:8787` by default. This is what a person runs to watch
their own conversations locally. It hosts the `claude-code-otlp` receiver too, when that adapter
is enabled.

`/api/status` reports the mode, the source, the last and the next refresh and the counts of the
last pass, and the list page shows the same. The page is up before the first pass, so a large
backfill does not look like a hung command.

`server` needs a local source. To serve a root that already exists, use `view`.

## view

Serves the conversations as a page on `ADDR`, `127.0.0.1:8787` by default, and **only reads**:
nothing in this process collects, parses or sends. It is the web host for a root that asz collect
filled, or one copied from another machine, or one a receiver wrote. The list is at `/` and
one conversation at `/c/{id}`. The page reads the folded chain on demand and caches nothing beyond
the process.

A conversation is drawn by Horizon's conversation renderer, the same one the SkyWalking UI uses
for the conversations the OAP holds, so the two draw a conversation identically. asz embeds the
renderer's build from a pinned Horizon commit, with Horizon's themes and fonts, so the page needs
nothing from the network but its own API. The page's address carries the reader's position, the
talk, the selected step and the stream being read, so a link lands on the same step. Beyond what
Horizon can show, the Evidence tab opens the landed record behind a step, since only asz has the
files.

A root with no conversations in it is refused rather than served empty: run `asz collect` first,
or `asz server` to collect and serve together.

The list shows what the head round's header counted — talks, model calls, subagents, Bash runs,
changes with the lines they added and removed, and open unresolved references — so it never folds a
conversation to draw a row. A count the head round does not carry shows a dash rather than a zero:
a round cut before that count existed does not know the answer.

## scenario

```text
asz scenario build FILE... --format {claude-code|sd} --out DIR [--at TIME] [--scale FACTOR] [--repeat N]
                           [--every D] [--pick {cycle|random}] [--seed N] [--through CHECKPOINT]
asz scenario check FILE [--format {claude-code|sd|all}] [--out DIR] [--at TIME] [--scale FACTOR]
```

`build` turns a scenario file into the input a session leaves behind, under `DIR`, with a
`DIR/asz.yaml` for the ordinary commands to collect and parse it. `check` runs the scenario as a
test against its expectation file, in every format, at every checkpoint. See
[Scenarios](../guides/scenario.md).

More than one `FILE` may be given, and a `FILE` that names a directory contributes every `.yaml`
file in it, except the expectation files. `--pick` says which of them each session comes from.

With `--every`, `build` does not stop. It writes one whole session, waits that long on the wall
clock, then writes the next, so it stands in for a client that keeps holding conversations. Each
session is stamped so its last record lands at the moment it was written, and carries an id no
earlier session has, so a feed that is stopped and started again is still collected. One line per
session:

```text
[14:22:31] assembly.yaml: c30736f2-ac0c-4a72-89b1-01a0844af62d (20 records, 15.4s)
```

## push

Sends every landed file and every round not yet sent to the OpenTelemetry logs receiver at
`export.otlp.endpoint`, one log record per file, then exits. One pass is all it does: a root that
keeps growing is sent by `asz collect`, which pushes at the end of every period. This command is
for a root that is already there, such as one copied from another machine or brought under a new
budget by `asz repack`. One line for the pass:

```text
[10:12:03] files=306 metrics=41 bytes=47.9MB wire=48.0MB requests=46 paused=0s errors=0 (1.1s)
```

`metrics` counts the spooled metrics requests sent, `wire` is what went out, the requests as
encoded, and `paused` is how long the pass waited for budget under
`export.otlp.max_bytes_per_minute`. A pass with errors exits non-zero; the files whose requests
failed are not recorded as sent and go again on the next pass. See
[Export over OpenTelemetry](export-otlp.md).

## glossary

Prints every name the model uses, what the runtime calls it, where in the runtime's records it is
found, and a note. A `—` in the runtime column means the model derives the concept and the runtime
has no word for it. Every command that prints names accepts `-terms native` to use the runtime's
words instead.

## verify

Checks every landed file against its digest and every round against its commit digest, for one
session or all of them, and binds every round to the landed files it consumed: a round whose file
is gone, or whose files no longer digest to what it consumed, is reported by round and sequence.
It reads the storage root only, so it works without the source files and without a collector, and
it exits non-zero when anything is wrong.

```text
checked 44 session(s), 5867 stream(s), 359292 records
checked 44 conversation chain(s), 44 round(s)
all landed data is contiguous and matches its digests
```

## version

Prints the version the binary was built with, the Go version, and the platform. A release build
says its version; a plain `go build` says `dev`.
