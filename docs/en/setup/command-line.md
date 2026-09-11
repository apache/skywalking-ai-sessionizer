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
| `-terms MODE` | Name things in the model's words (`unified`, the default), the runtime's (`native`), or `both`. Every command accepts it, but only the plain output of `asz conversation` uses it. The asz.view document and the page have no such switch. |

## sources

Discovers sessions under the source root and counts their files without landing anything. The
first line names the source root it resolved. Sessions removed by the include and exclude filters
are counted on the `filtered` line. Then comes one row per session: directories, execution streams,
child-agent sidecars, workflow journals and manifests. A line with the total of sessions and streams
follows the table.

After the total, a list names every session whose files span more than one source directory, with
its number of directories. It shows that discovery groups files by session and not by directory, as
[Discovery is session-first](../adapters/claude-code.md#discovery-is-session-first) explains. The
list is absent when no session spans more than one.

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
| `busy` | how many times another process on the same root held a session, a conversation's chain or the export state this pass needed, absent when 0 |
| `pushed` | files and rounds sent, absent when no endpoint is named |
| `metrics` | spooled metrics requests sent |
| `errors` | listed on standard error after the line |

`busy` is not an error. Running two pipelines on one root is supported, and the process that holds
a lock is doing the same work. A session whose chain was held is parsed on a later pass. A single
pass has no later pass, so there that session is listed as an error. So is an export state that
another process still holds after two minutes of waiting, because then nothing was sent.

Anything else the pass could not do is listed on standard error under the line, one `error:` line
each: a session that failed to parse, a request the receiver refused. A watching collector goes on
to the next pass. A single pass exits non-zero, so an unattended backfill can be read by its exit
status.

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

`ROUND` is the round written, or `-` when nothing changed. A round is cut at the byte budget, so
one pass may write several, and the row then shows the last. `SEQ` is the landed sequence the
conversation now covers.

`NODES`, `RELS` and `UNRES` count one round: the frames that round wrote. A round writes only what
is new or changed, so after the first round these numbers are small, and they are 0 when no round
is written. `NODES` and `RELS` include tombstones, the entities the round removes. `UNRES` includes
the entries the round marks `resolved`, so it is not the number still open.

`TALKS`, `RUNS` and `STEPS` count the whole conversation through `SEQ`. `TOOLS` and `CHILDREN` also
count the whole conversation, and carry the denominator that
[Qualification](../concepts-and-designs/conversation-assembly.md#qualification) asks for: how many
tool calls and child-agent launches were joined, out of how many exist. The number of unresolved
references still open has no denominator. It is the round header's `unresolved`, the
`summary.unresolved` of the [asz.view](../formats/asz-view.md) document, and the `still open` count
of `asz conversation`.

The command exits non-zero when any session failed to parse, so a loop or a CI step can read the
status instead of the log.

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
one conversation at `/c/{id}`.

A conversation is drawn by Horizon's conversation renderer, the same one the SkyWalking UI uses
for the conversations the OAP holds, so the two draw a conversation identically. asz embeds the
renderer's build from a pinned Horizon commit, with Horizon's themes and fonts, so the page needs
nothing from the network but its own API. The page's address carries the reader's position, the
talk, the selected step and the stream being read, so a link lands on the same step. Beyond what
Horizon can show, the Evidence tab opens the landed record behind a step, since only asz has the
files.

A root with no conversations in it is refused rather than served empty: run `asz collect` first,
or `asz server` to collect and serve together.

The list folds every conversation to draw its rows. Talks, steps, open unresolved references and
the time span are read from the fold. Model calls, subagents, Bash runs, and changes with the lines
they added and removed are what the head round's header counted. A header count the head round does
not carry shows a dash rather than a zero: a round cut before that count existed does not know the
answer. A conversation whose chain has no round that can be folded is left out of the list.

The page reads everything it shows from a JSON API on the same address:

| Path | Returns |
| --- | --- |
| `/api/status` | the mode and the last refresh. See [server](#server) and below. |
| `/api/conversations` | one row per conversation, for the list |
| `/api/c/{id}/view` | the whole [asz.view](../formats/asz-view.md) document, which the renderer draws alone |
| `/api/c/{id}/record/{seq}/{row}` | one landed record, whole. This is what the Evidence tab shows. |
| `/api/glossary` | what the runtime calls each name the model uses, as [glossary](#glossary) prints it |

There is no endpoint per talk. Beyond the document, a conversation page asks the API for two
things: `/api/glossary`, once as it loads, and a landed record, only when the reader opens one in
the Evidence tab.

A row of `/api/conversations` carries `id`, `title`, `talks`, `steps`, `streams`, `segments`,
`rounds`, `from`, `to` and `unresolved`, the references still open, then `changes`, `lines_added`,
`lines_removed`, `llm_calls`, `subagents` and `bash_runs`. A null in the last six means the head
round was written before that count existed. `from` and `to` are unix milliseconds: when the
session began and its last activity.

`/api/c/{id}/record/{seq}/{row}` returns the [Session Data record](../formats/session-data.md#record)
as it was landed, with its `dropped` list. `seq` names the landed file and `row` the record in it,
counting from 1. The record is read from disk on every request and never cached, because it is
wanted only when someone opens it. The answer is 404 when the conversation, the sequence or the row
does not exist.

`/api/glossary` returns three keys. `dialect` says whose vocabulary the terms are in. `terms` is
keyed by the model's name, and each entry carries the runtime's `native` name, `where` the runtime
records it, and a `note`. `fields` describes the fields Session Data defines itself, such as `ord`,
`parts` and `usage`, in one line each. A field that names something the model names, such as
`stream`, `call` or `run`, is described under `terms` instead. The keys of a landed file's last
line, `t`, `records` and `digest`, are in neither.

The page uses the glossary to explain the landed record the Evidence tab shows. Every key found in
`terms` or `fields` carries a `?`, which opens what the key means. For a name the model uses, that
is the runtime's word or the fact that the runtime has none, where to look in the runtime's records,
the note, and the dialect. For a field Session Data defines, it is the field's line from `fields`.
The page has no switch to the runtime's words.

The page folds a conversation when it is first asked for, and keeps the fold in memory until the
head round on disk moves. The asz.view document is built once per fold. Nothing is written to disk.
A read cache on disk would cost more than it saves. Measured on the largest session of a corpus of
62 real sessions, 53,106 nodes and 922 talks, folding the whole chain took 302 milliseconds,
building the talk list 1 millisecond, and walking one talk's subtree 13 microseconds at the 99th
percentile.
[asz.view](../formats/asz-view.md#reading-it) gives the size and build time of a whole document.

Another process can write the root while the page reads it: an `asz collect` running beside it, or
a receiver. Nothing needs a restart. Every request for a conversation lists its rounds directory,
and folds again when the head round has moved. A landed file that arrives after the fold is found
by a new scan of the session directory. That scan runs at most once a second, so a request for a
sequence that does not exist does not walk the directory every time. The root itself is checked on
the collector interval, 5 seconds by default. `/api/status` reports as `last_refresh` the last time
a head round or the size of its file changed, and the list page reloads when that time moves.

A round file is created under its final name and written in place, so a listing can show a round
before all its bytes are there. The fold then stops before that round. For that one read, the
document names the round in `summary.problems`, and its `summary.state` is `mismatch`, the same
state a failed digest gives. The next read folds it. The cost of checking the head on every read,
and of folding again while a conversation is still being written, has not been measured.

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
has no word for it. With `-terms native`, the plain output of `asz conversation` uses the
runtime's words instead.

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

Contiguous means that, in every stream, the landed records continue the source's line numbers and
byte offsets with no gap. Each record carries the line it came from, `ord`, the byte it started at,
`off`, and its size, `bytes`. The next new record must be on the next line and must start at
`off + bytes + 1`, the one being the newline the source had. That proves every source byte up to
the cursor is accounted for, without the source. A skipped line is listed as an `ord gap`, and
bytes no record accounts for as a `byte gap`, each with its file and row. The check then goes on.

A landed file's last line carries a digest of every line before it, so reading the file catches
one that was edited or cut short. Such a file ends the check with an error that names it.

A record that repeats a range already landed is counted, not reported as a problem. verify prints
`N record(s) landed more than once by an interrupted pass; assembly removes them`. A file lands
before its cursor is committed, so an interrupted pass repeats data rather than losing it.
[Storage Root](../formats/storage-root.md#landed-files) explains the order.

## version

Prints the version the binary was built with, the Go version, and the platform. A release build
says its version; a plain `go build` says `dev`.
