# Command Line Internals

The details behind [Command Line](../setup/command-line.md): what each report counts, how the page
reads a storage root, how `verify` proves a stream whole, and how a scenario root behaves.

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

With the `changes` adapter enabled, a second table lists the sessions the plugin has
written change records for, with their streams and workspace.

With the `claude-code-provider` adapter enabled, it names the directory Claude Code writes provider
bodies to, counts the requests and responses there, and lists the sessions the requests name. A
response names no session, so it is counted only. See
[Claude Code Provider Bodies](../setup/claude-code-provider-bodies.md).

## collect

The pipeline. One pass does three things, in this order:

1. **land** what is new from every enabled local source into the storage root, and update the index
2. **parse** every session that moved, writing the rounds
3. **send** what the export block asks for, when `export.otlp.endpoint` is set

The order matters. The push runs last so a pass sends the rounds it has just written, rather than
leaving them for the next one.

With `storage.retention.metadata_only_after` set, a pass then keeps only the metadata of every
conversation idle for longer, sent or not: it removes the landed files and leaves a skeleton in
each one's place. With `storage.retention.retire_after` set, a pass first retires every
conversation idle for longer: everything of it is removed, and it is never collected again. Before
it lands anything, every pass finishes a removal or a retirement an earlier one left in progress,
whatever the settings say now. A scenario root keeps its own removal, and retention never runs
there. See [Retention](../formats/storage-root.md#retention). The startup lines say the
retention:

```text
retention: a conversation idle for 72h0m0s keeps only its metadata; its content is removed, and only a receiver that accepted it still has it
retention: a conversation idle for 2160h0m0s is retired: everything of it is removed, and it is never collected again, even when its runtime resumes it
warning  : Kiro keeps a session until it is deleted; storage.retention.retire_after is 90 days. A session resumed after it is retired is not collected.
```

When both durations are the same, a conversation is retired without first keeping only its metadata.
Only the second retention line is said. `asz parse` and `asz push` finish a retirement left in
progress before they do anything else.

With `retire_after` set, `warning  :` lines follow the retention lines.
The Kiro line above appears when `kiro-local` is enabled.
For `claude-code-local`, asz reports what it could inspect in Claude Code's settings.
It says that effective retention was not determined. It also warns when `retire_after` is shorter
than the inspected `cleanupPeriodDays` or the reported default of 30 days.
The inspected value does not establish Claude Code's effective retention.
See [Retirement](../formats/storage-root.md#retirement) for the inspection's limits.
`asz sources` prints the same lines for each runtime. The list page shows a retention warning in
its refresh status and the full text in its title. The first pass writes the lines to standard
error once per process. With `retire_after` off, none of these lines is shown.

When metrics are turned on after a conversation kept only its metadata, the skeletons give its
tokens, and not whether an execution was a call to an MCP server. The first pass of a process says,
for each session, how many distinct execution observations its skeletons keep that no pass counted.
The count does not say how many of them were MCP calls:

```text
  metrics: <session>: 4 execution observations lack retained details; their MCP metrics cannot be derived
```

In watch mode, the default, it repeats every `interval`; with `-once` or `mode: once` it makes one
pass and exits. Every enabled local adapter is read in the same pass, so one watching source never
keeps another from running. When the `langsmith-ingest` adapter is enabled, its receiver listens
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
| `rejected` | records and points a receiver took and rejected, absent when 0. It also gives an `error:` line, because they are never sent again. |
| `removed` | scenario sessions removed, absent when 0. See [A scenario root](#a-scenario-root). |
| `retention` | landed files `storage.retention.metadata_only_after` removed, absent when 0. See [Retention](../formats/storage-root.md#retention). |
| `retired` | conversations `storage.retention.retire_after` retired, absent when 0. See [Retirement](../formats/storage-root.md#retirement). |
| `errors` | listed on standard error after the line |

`busy` is not an error. Running two pipelines on one root is supported, and the process that holds
a lock is doing the same work. A scenario root is the exception, below. A session whose chain was
held is parsed on a later pass. A single pass has no later pass, so there that session is listed
as an error. So is an export state that another process still holds after two minutes of waiting,
because then nothing was sent.

Each pass holds the root lock, `_root/.lock`, from the first step to the last, so two commands
never write the same root at once. `asz parse`, `asz push` and `asz repack` hold it for their run,
and `asz scenario build` for each session it builds. The LangSmith receiver is the one writer
outside a pass. It only adds requests to `_langsmith/`, and a pass takes them under that
directory's own lock.
When another command holds it, a watching pipeline prints `waiting  : another command holds
<root>/_root/.lock; this pass does nothing until it is released` once and skips the pass. A single
pass waits up to two minutes for it, then exits non-zero with nothing done.

Anything else the pass could not do is listed on standard error under the line, one `error:` line
each: a session that failed to parse, a request the receiver refused. A watching collector goes on
to the next pass. A single pass exits non-zero, so an unattended backfill can be read by its exit
status.

## A scenario root

A root a `claude-code` scenario build wrote holds `_scenario/`. There a pass does one more thing,
after the send. It removes each session the build marked that is now sent in full, by the policy in
the session's marker. [Removal](scenario.md#removal-a-session-goes-once-it-is-sent) says
what that takes. A root no build wrote has no `_scenario/`, and no session in it is ever removed
whole. Retention, when it is set, keeps only the metadata of an idle conversation in such a root,
and in one an sd build wrote. It never runs over a root with `_scenario/`.

When the root holds `_scenario/` at the start, a `scenario :` line says what the removal will do.
It asks the check each removal pass makes before it looks at any session, so it never promises a
removal the configuration prevents. When nothing in the configuration prevents one, it prints:

```text
scenario : a session a build marked is removed by its policy once all of it is sent
```

Otherwise it prints `scenario : no session is removed: <reason>`, where the reason names what keeps
every marked session, such as `no export endpoint; nothing is sent, so nothing is removed`. A root
that gets `_scenario/` only later, from a feed started after the pipeline, has no such line at the
start.

Such a root takes one pipeline at a time. The first pass that finds `_scenario/` takes
`_scenario/.lock` and holds it until the process ends. It prints the lock, then the same words as
the line above:

```text
scenario : this pipeline holds <root>/_scenario/.lock; a session a build marked is removed by its policy once all of it is sent
```

A second watching pipeline over the same root prints `waiting  : another pipeline holds
<root>/_scenario/.lock; this one does nothing until it is released` once. It then lands, parses and
sends nothing until the lock is free. A single pass waits up to two minutes for the lock, then exits
non-zero with nothing done. A second pipeline that went on could land a removed session again, or
parse evidence that is gone.

A marked session the pass cannot prove sent is kept. When a person has to act, the reason is
printed once per process on standard error:

```text
  kept: every marked session: no export endpoint; nothing is sent, so nothing is removed
  kept: 3189c1f0-9ec4-4bd2-88dc-8eda88ac6db3: the receiver rejected records of 3189c1f0-…/streams/main/transcript-…-000001.sd
```

`every marked session` means the reason holds for all of them. A kept session is not an error, and
a single pass does not exit non-zero for it. A session that is only not sent yet, or not yet old
enough for its policy, is kept with no line. A removal that stops part way on a check, such as a
source file that changed since the build wrote it, is an `error:` line that names the file. That
session is left for a person.

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
exceeds the configured `max_delta_bytes`, header and closing line included, then assembles
`DEST`'s chains. The one exception is a record larger than the budget: it is carried whole, in a
file of its own, which is then larger than the budget. Every record keeps its bytes and its order,
and records of two files are cut into one only when the files are next to each other and their
headers are the same apart from `seq` and `at`. Every cursor comes along so collection can
continue into `DEST`, the one a second source of a stream keeps too, such as a second recorder
directory. Each names the new file that holds the last record its source landed. A cursor whose
`origin` is relative to the storage root, a recorder inside it, gets the origin and the name a
collector into `DEST` gives it. The index and the chains are rebuilt there, because a round
addresses records by file and line and the old references name positions that no longer exist.
Each new file keeps the time the newest file its records came from was collected, and the files
are numbered in the order their records landed, so retention finds a session as idle in `DEST` as
in the root it was repacked from. `DEST` must be a new or empty directory outside
the storage root. A populated destination is refused before any session is written, because its
chains bind to the original landed files. One inside the root is refused too, because every
command would read it as a session of the root. If a repack is interrupted, retry into another
empty directory. The repack holds the root lock of the root it reads, and waits for a pass that
holds it. It holds `DEST`'s lock too, without waiting, and checks `DEST` again under it: a command
that holds a new or empty root is writing into it. A session retention removed files of is
refused before anything is written, and so is one with a removal in progress: a skeleton keeps no
records to re-cut. One row per session:

```text
SESSION                               FILES IN  FILES OUT  RECORDS  BYTES
0438c73b-2367-4ed5-9de3-13ef9a17ed01  305       312        21119    42.1MB
```

This is how an existing root is brought under a new budget after `max_delta_bytes` changes, and
it needs no source files.

## server

The pipeline and the page in one process: it lands, parses and sends on the collector interval,
and serves the result on `ADDR`, `127.0.0.1:8787` by default. This is what a person runs to watch
their own conversations locally. It hosts the `langsmith-ingest` receiver too, when that adapter
is enabled.

`/api/status` reports the mode, the source, the last and the next refresh and the counts of the
last pass, and the list page shows the same. The page is up before the first pass, so a large
backfill does not look like a hung command.

Over a [scenario root](#a-scenario-root), it removes sent sessions as `collect` does, and the page
stops listing each one it removes.

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
| `/api/c/{id}/view` | the [asz.view](../formats/asz-view.md) document, less what this reader withholds, which the renderer draws alone |
| `/api/c/{id}/record/{seq}/{row}` | one landed record, whole unless withheld. This is what the Evidence tab shows. |
| `/api/c/{id}/files?session=&seq=` | provider body files whole, by sequence, at most 32 and 64 MiB a request, as base64 in a JSON array. A file larger than that on its own is served alone, and the page asks again for what an answer left out. This is what the Prompt tab reads to rebuild a call's provider bodies. A sequence with no landed file is left out rather than refused. Any other file is refused. A file [retention](../formats/storage-root.md#retention) removed is served as its skeleton, without compression, with `kept`: `bodies`, the body of each record the session's store kept as a chain tip, by record id, and `pieces`, each removed piece of it a later body cites, by SHA-256, both base64. |
| `/api/glossary` | what the runtime calls each name the model uses, as [glossary](../setup/command-line.md#glossary) prints it |

There is no endpoint per talk. Beyond the document, a conversation page asks the API for three
things: `/api/glossary`, once as it loads; a landed record, only when the reader opens one in the
Evidence tab; and a session's files, only when the reader opens the Prompt tab on a call whose
provider bodies landed.

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

The three `/api/c/{id}` paths withhold what `view.hide` names. With `view.hide_override` on they
take `hide`, the flags this request withholds in place of `view.hide`, as
[Withholding](../formats/asz-view.md#withholding) says. A record that carries one keeps its envelope
and loses its content. A reader that withholds anything is served no provider body: the files
endpoint and the record endpoint, for a record of a body file, answer 403. The answer is 400 for
`hide` on an instance with `view.hide_override` off, for a name that cannot be withheld, for `hide`
spelled another way, for a query that does not parse, and from the files endpoint for a file that is
not a provider body or whose header does not read.

`/api/glossary` takes an optional `dialect`, and without one answers in the vocabulary the root's
own landed files were read in — a root of LangChain conversations is not described in Claude Code's
words. A root holding both answers with whichever its sessions name first, and lists the rest under
`dialects`; the page asks once as it loads, so describing two runtimes at once would need it to ask
per conversation instead.

It returns four keys. `dialect` says whose vocabulary the terms are in. `dialects` says what else
the root holds. `terms` is
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
head round on disk moves. The asz.view document is built once per fold for each set of names a
reader withholds. Nothing is written to disk. A read cache on disk would cost more than it saves.
Measured on the largest session of a corpus of 62 real sessions, 53,106 nodes and 922 talks, folding
the whole chain took 302 milliseconds, building the talk list 1 millisecond, and walking one talk's
subtree 13 microseconds at the 99th percentile. [asz.view](../formats/asz-view.md#reading-it) gives
the size and build time of a whole document.

Another process can write the root while the page reads it: an `asz collect` running beside it, or
a receiver. Nothing needs a restart. Every request for a conversation lists its rounds directory,
and folds again when the head round has moved. A landed file that arrives after the fold is found
by a new scan of the session directory. That scan runs at most once a second, so a request for a
sequence that does not exist does not walk the directory every time. The root itself is checked on
the collector interval, 10 minutes by default. `/api/status` reports as `last_refresh` the last time
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
                           [--remove {immediately|DURATION}]
asz scenario check FILE [--format {claude-code|sd|all}] [--out DIR] [--at TIME] [--scale FACTOR]
```

`build` turns a scenario file into the input a session leaves behind, under `DIR`, with a
`DIR/asz.yaml` for the ordinary commands to collect and parse it. `check` runs the scenario as a
test against its expectation file, in every format, at every checkpoint. See
[Scenarios](scenario.md).

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

A `claude-code` build also writes a marker for each session, with the policy `--remove` names:
`immediately`, the default, or a duration such as `30m`, `24h` or `7d`, counted from the session's
last record. `asz collect` and `asz server` remove a marked session by that policy once all of it
is sent, as [Removal](scenario.md#removal-a-session-goes-once-it-is-sent) describes. The
build prints the policy after `config`, and a feed prints it in its header:

```text
remove   : 24h after its last record, once sent
```

`--remove` with `--format sd` is refused, because an sd build writes no source and a scenario's
removal never removes its sessions. Retention, when it is set, still applies to its root, as to a
real one, and retirement removes a session there whole. A build of a session retired in its root is
refused. `check` refuses `--remove`, and `build` refuses zero, a negative duration and
anything it cannot read. Each refusal exits with status 2 and writes nothing.

## push

Sends every landed file and every round not yet sent to the OpenTelemetry logs receiver at
`export.otlp.endpoint`, one log record per file, then exits. One pass is all it does: a root that
keeps growing is sent by `asz collect`, which pushes at the end of every period. This command is
for a root that is already there, such as one copied from another machine or brought under a new
budget by `asz repack`. It starts no removal and no retirement, even over a [scenario
root](#a-scenario-root). It finishes a retirement a pass left unfinished, as every command that
writes to a root does first.
One line for the pass:

```text
[10:12:03] files=306 metrics=41 bytes=47.9MB wire=48.0MB requests=46 paused=0s rejected=0 errors=0 (1.1s)
```

`metrics` counts the spooled metrics requests sent, `wire` is what went out, the requests as
encoded, and `paused` is how long the pass waited for budget under
`export.otlp.max_bytes_per_minute`. A pass with errors exits non-zero; the files whose requests
failed are not recorded as sent and go again on the next pass. See
[Export over OpenTelemetry](../setup/export-otlp.md).

A session goes to the endpoint, the configured service name and the instance it was first sent
under. After one of them changes, a session sent before is held: nothing more of it is sent, and
the pass says so on standard error and exits non-zero. `asz collect` lists the same words as an `error:` line on every
pass:

```text
  held: session 3189c1f0-9ec4-4bd2-88dc-8eda88ac6db3 was sent to grpc:%2F%2F10.0.0.1:11800|Claude%20Code|me@laptop and this pass sends to grpc:%2F%2F10.0.0.2:11800|Claude%20Code|me@laptop; its later files wait, since a receiver holds a session whole. Set the endpoint, service and instance back to send them
```

[One owner per session](../formats/otlp.md#one-owner-per-session) says why.

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

A file retention removed is not verified: only its skeleton is there. The checks go on through the
skeleton, which keeps every record's line, byte and size, and a round is bound to the digest the
file had, which the skeleton recorded. Removed files are counted on a line of their own, by what
the receiver confirmed of them:

```text
12 file(s) were removed by retention and are not verified: the receiver accepted 11, accepted 0 with rejected records, and confirmed nothing of 1
```

Contiguous means that, in every stream, the landed records continue the source's line numbers and
byte offsets with no gap. Each record carries the line it came from, `ord`, the byte it started at,
`off`, and its size, `bytes`. The first record must be line 1 and must start at byte 0. Each next
new record must be on the next line and must start at `off + bytes + 1`, the one being the newline
the source had. Where the stream has an [append cursor](../formats/storage-root.md#append-cursors),
the records must reach the line and the byte the cursor says the collector read to. Together that
proves every source byte up to the cursor is accounted for, without the source. A skipped line is
listed as an `ord gap` and bytes no record accounts for as a `byte gap`, each with its file and
row. Records that stop before the cursor are listed as an `end gap`, with the cursor's position and
theirs. The check then goes on.

So in a stream with an append cursor, a lost landed file shows, whether it was the stream's first,
a middle or the last, and whether or not a round consumed it. A root that was never parsed, or
whose rounds were removed, fails on such a loss too. There are two exceptions, and in both only a
round that consumed the file shows the loss. A lost file leaves no gap when another landed file
holds the same records, as after an interrupted pass. A cursor stays in the storage root and never
travels, so in a root rebuilt from a push the check stops at the last landed record, and a lost
last file leaves no gap there.

Which streams have an append cursor depends on what landed them. `claude-code-local` gives one to a
transcript and a workflow journal, and `changes` to the plugin's change records.
`claude-code-local` reads a child agent's sidecar, a workflow manifest and a workflow script whole,
with a snapshot cursor. So in a root it collected, a lost file of one of them is found only by the
round chain, and a root with no rounds does not show it. A scenario `sd` build gives each of them
an append cursor that counts records, so there such a loss is an end gap too.

A cursor behind the records is what an interrupted pass leaves, and is not a problem. Measured on
2026-09-11 on one machine's storage root of 60 sessions, all 6,080 streams began at line 1 and
byte 0, and none of the 2,995 append cursors was ahead of its landed records. So on that root,
neither the rule for the first record nor the rule for the cursor reported a problem that was not
there.

A landed file's last line carries a digest of every line before it, so reading the file catches
one that was edited or cut short. Such a file ends the check with an error that names it.

A record that repeats a range already landed is counted, not reported as a problem. verify prints
`N record(s) landed more than once by an interrupted pass; assembly removes them`. A file lands
before its cursor is committed, so an interrupted pass repeats data rather than losing it.
[Storage Root](../formats/storage-root.md#landed-files) explains the order.

A part whose payload was [too large to land](../formats/session-data.md#too-large-to-land) is
counted the same way, with the provider bodies that cannot be rebuilt because of it: verify prints
`N part(s) were too large to land and hold their size and digest only; M provider body(ies) cannot
be rebuilt for it`. The collector left the payload out on purpose, so the file fits its budget. A
body that holds or refers to such a piece is not damage: its references are still checked, and its
record's `bytes` and `sha` are still held to its manifest. Only the comparison of its rebuilt bytes
with its digest cannot be made.
