# Scenarios

A scenario is a short YAML description of a conversation, in the model's own words: a person's
input, a call with its fragments, a tool and its result, a child agent, a context reset. `asz
scenario build` turns it into the input a real session leaves behind, so the ordinary commands
collect and parse it. `asz scenario check` runs it as a test.

The `.sf` side is never mocked. A scenario produces evidence; the ordinary parser produces the
rounds. That is what makes a scenario both a fixture generator and a conformance test: built as
the runtime's own files and collected through its adapter, or landed directly as Session Data, it
must fold to the same conversation.

## Generate, then load, parse and export

`build` only generates. It writes the input and `DIR/asz.yaml`, whose storage root is `DIR` and
whose adapter source is `DIR/_source`, then stops; the ordinary commands do the rest, each one
inspectable on disk before the next.

```sh
asz scenario build FILE --format claude-code --out DIR      # generate the runtime's files, under DIR/_source
asz collect -once -config DIR/asz.yaml                      # load and parse: the real adapter and the real parser
asz verify -config DIR/asz.yaml                             # every digest and every chain
asz server -config DIR/asz.yaml                             # the page, on 127.0.0.1:8787
asz push -config DIR/asz.yaml                               # export: every file to an OTLP receiver
```

With `--format sd` the Session Data is landed by the build itself, so the collect step is skipped
and the rest is the same. To push, name the receiver in `DIR/asz.yaml`; `build` leaves the block
there, commented:

```yaml
export:
  otlp:
    protocol: grpc
    endpoint: 127.0.0.1:11800
```

Once a receiver is named, `asz collect` and `asz server` also remove each session a `claude-code`
build wrote, once all of it is sent, as [Removal](#removal-a-session-goes-once-it-is-sent)
describes. `asz push` sends and never removes.

The pushed records say where they came from. A `claude-code` build is landed by the Claude Code
adapter and is attributed to `Claude Code`; an `sd` build carries the `mock/1` dialect and is
attributed to `Mock Agent`, so a receiver never lists an invented conversation as a real one.

A demo corpus is one scenario repeated: `--repeat 20` builds twenty sessions end to end on the
clock, the first with the scenario's own id and the rest counted up from it, and `--at
2026-06-01T09:00:00Z --scale 60` puts them on a day rather than in a burst. `build` refuses a
`DIR/asz.yaml` it did not write, so a directory holds one configuration; what a person appends to
it, such as the export block, is kept across builds.

| Flag | Meaning |
| --- | --- |
| `--format` | `claude-code` or `sd` |
| `--out` | the directory; it ends as a storage root |
| `--at` | the base time, RFC 3339, or `now` (default) |
| `--scale` | multiplies every delta; `60` turns a scenario typed in seconds into minutes |
| `--interval` | overrides the scenario's interval |
| `--repeat N` | N sessions end to end on the clock, each with its own id |
| `--every D` | keep building: one session every D of wall clock, until the command is stopped |
| `--pick MODE` | with a set of scenarios, `cycle` through them in order or take a `random` one |
| `--seed N` | seeds `--pick random`; `0`, the default, varies with each run |
| `--through NAME` | only the steps up to the checkpoint NAME |
| `--remove POLICY` | `claude-code` only: when a pipeline removes a session once all of it is sent, `immediately` (default) or a duration such as `24h` or `7d` after its last record. See [Removal](#removal-a-session-goes-once-it-is-sent) |

With a fixed `--at`, every file is identical on every run.

## A feed: keep sending conversations

`--repeat` writes its sessions in one burst and stops. `--every` does not stop. It builds one whole
session, waits that long on the wall clock, builds the next, and goes on until it is stopped. That
is a mock client: run it beside a watching collector and conversations keep arriving.

```sh
asz scenario build tests/scenarios/assembly.yaml --format claude-code --out DIR --every 30s &
asz server -config DIR/asz.yaml                             # collect, parse and serve, on the interval
```

`server` refreshes on the interval because the configuration the build writes says `mode: watch`.
To feed a receiver instead of a page, name it under `export.otlp` and run `asz collect`, which
sends at the end of every period. With a receiver named, under either command, each session of the
feed is removed once all of it is sent, or later with `--remove`.
[Removal](#removal-a-session-goes-once-it-is-sent) says how.

Two things differ from a one-shot build, and both are what a live feed needs.

A session is stamped so its **last record lands at the moment it was written**. A real session that
has just ended has its last record now, and a receiver reads nothing stamped ahead of the clock it
reads it with. A one-shot build starts at `--at` instead and runs forward from there.

Each session gets an id **no earlier session has**, taken from the moment it was written rather
than from a counter. A counter starts again at one when the feed is restarted. The repeated id
would rewrite the same source file in place, and the collector, which reads forward from where it
stopped, would see no growth and land nothing at all.

The configuration the build writes says `mode: watch` rather than `mode: once`, because the source
keeps growing.

### A set of scenarios

More than one file may be given, and a directory contributes every `.yaml` file in it, except the
expectation files. `--pick cycle`, the default, walks the list in the order it was written and
starts again at the top. `--pick random` takes one each time; `--seed` makes that order repeatable.

```sh
asz scenario build tests/scenarios --format claude-code --out DIR --every 1m --pick random
asz scenario build a.yaml b.yaml c.yaml --format sd --out DIR --every 10s
```

With `--format sd` a feed lands its Session Data itself, so there is nothing for a collector to
watch and `server` does not notice the later sessions. Run `asz parse -config DIR/asz.yaml` beside
it, or feed in `claude-code` format, which goes through the real adapter and is the closer stand-in
for a client anyway.

`--every` with `--repeat N` stops after N sessions rather than running on, and does not wait after
the last one. The period must be at least `1ms`: a feed id carries the moment it was written, in
milliseconds, so a shorter period could not give every session an id of its own.

## Removal: a session goes once it is sent

A `claude-code` build also says when a pipeline may remove each session it writes. It writes a
marker for the session, `DIR/_source/.asz-scenario/<session-id>.json`, after every other file of
it. The marker names the policy and lists each file the build wrote, with its size and SHA-256.
`--remove` sets the policy:

| `--remove` | A session goes at the first pass where all of it is sent and |
| --- | --- |
| `immediately`, the default | nothing more is needed |
| a duration, such as `30m`, `24h` or `7d` | its last record is at least that old |

In a watching pipeline, a session whose last call may go on in a later file waits two minutes
before its metrics are derived, so it goes about two minutes after it lands. A single pass does not
wait.

`asz collect` and `asz server` remove a session at the end of a pass, after the send, once every
landed file, every round and every metrics request of it is recorded as sent to the receiver they
send to. They remove the source files the build wrote, the landed files, the chain, the spool files
and the session's lines in the state files, and the marker last. Once removed, the conversation
leaves the page, and the receiver holds the only copy.
[Storage Root](../formats/storage-root.md#a-scenario-root) gives the order and the reason for each
step.

The pipeline removes only over the storage root the build wrote, `DIR` itself, where the build
creates `_scenario/`. The `DIR/asz.yaml` the build writes sets `storage.root` to `DIR`. A pipeline
with another `storage.root` lands and sends the sessions but never removes them, and prints no line
about it.

A duration counts from the session's last record, never from when it landed, so the same landed
data always gives the same answer. A feed stamps each session so its last record falls when it was
written, so a feed's session stays about that long after it was written. A one-shot build with an
old `--at` is already past its duration, and goes as soon as it is sent. `d` counts whole days, as
`metrics_lookback` does. One run applies one policy to every session it writes.

The policy belongs to the scenario, never to the product. The configuration has no removal
setting, and a session no build marked, which is every real Claude Code session, is never removed.
An sd build writes no source and no marker, so its sessions are never removed, and `--remove` with
`--format sd` is refused.

Nothing is removed that the pipeline cannot prove sent. Each of these keeps every marked session,
and the pass says why once, on standard error, as `kept: every marked session: <reason>`:

- There is no `export.otlp.endpoint`, so nothing is sent.
- `export.otlp.logs` or `export.otlp.metrics` is switched off.
- The `claude-code-changes` adapter is switched off, or reads anything but
  `DIR/_source/plugins/data`, where the build wrote the plugin's output.
- `push.state` in the root records files sent to another receiver, or to one it cannot name.
- The source directory is one of the places Claude Code keeps its files, holds one, or lies inside
  one. All three are checked, whichever one the environment selects: what `CLAUDE_CONFIG_DIR`
  names, `XDG_CONFIG_HOME/claude` and `~/.claude`.
- `DIR/_removed` is a symbolic link, or not a directory.

One session is kept, with a `kept: <session-id>: <reason>` line, when something about it needs a
person. Examples are a receiver that rejected records of it, a file the build wrote that has
changed or is missing, and a file for it that the build did not write. A kept line is not an error.
Such a session stays until a person removes it. A session that is only not sent yet, or not old
enough, is kept without a line.

One pipeline works on a root at a time. A pipeline over a root a `claude-code` build wrote holds
`DIR/_scenario/.lock` for as long as it runs, and a second one waits and says so.
[collect](../setup/command-line.md#a-scenario-root) shows what each prints.

A build refuses to write a session whose marker says a pipeline is removing it. Building a
one-shot or `--repeat` scenario into the same directory again, after its sessions were removed,
writes the same ids again. The pipeline lands and sends them as new, under new file names, so the
receiver is sent them a second time. A feed never reuses an id.

## The scenario

A `claude-code` build writes `<`, `>` and `&` literally in its source JSON strings, as Claude Code
does, and writes U+2028 and U+2029 as the characters. In scenario YAML, write the last two as
`\u2028` and `\u2029` in double-quoted strings. This keeps them visible in an editor; a plain YAML
scalar treats the literal characters as line breaks.

A workflow's run id starts with `wf_`. Its name supplies the rest: characters other than ASCII
letters, digits, `_` and `-` become hyphens. If that result does not start with a letter or digit,
`run-` is added first. The script's file name uses the same name. A build refuses two workflows
whose names produce the same run id, ignoring letter case. `source-bytes.yaml` includes the name
`<all> & report` to check punctuation at the start as well as inside a name.

```yaml
session: mock-build-and-check       # optional; default derived from the steps
title: build and check              # optional
interval: 1s                        # the gap between steps, in every stream
steps:
  - input: run the build            # a person's message: opens a run and a talk
  - inject: {type: skill_listing, text: "skills: 1"}
    after: 100ms                    # this step's own delta since the previous one
  - call:                           # one provider call, in fragments
      thinking: unavailable         # a reasoning part with no text; any other value is the text
      text: Building now.
      tool: {id: t1, name: Bash, input: {command: make build}, result: {text: build succeeded, after: 800ms}}
      usage: {in: 2, out: 50, cache_read: 900, cache_write: 100}
    checkpoint: built               # a point a test can stop at
  - call:
      agent:                        # a child agent: the call, its acknowledgement, the child's own stream
        name: checker
        prompt: check the tests
        after: 1s                   # the child's first record, relative to the call
        steps: [{call: {text: Tests pass.}}]
        notify: true                # the runtime reports the child finished: a second run
  - call: {text: Build passed and tests are green.}
  - error: "API Error: Connection lost mid-response."   # an assistant-role message the client made
  - reset: {summary: "Summary: the build was run and checked."}
```

Every step is exactly one of these, plus an optional `after`, `checkpoint` and `lost`:

| Step | What it writes |
| --- | --- |
| `input` | a person's message; `trigger: external`; opens a run and a talk |
| `queued: {text, mode}` | input that exists only as a queued attachment; `mode` is `prompt` or `task-notification` |
| `inject: {type, text}` | material the harness put into context, of any type |
| `call` | fragments in this order: `thinking`, `text`, then one of `tool`, `agent`, `skill`, `workflow`; `usage` on every fragment; the last carries the stop reason |
| `result: {of, text, after, failed, string}` | a tool result arriving on its own, for a tool whose call gave none |
| `error` | a synthetic message |
| `reset: {summary}` | a context reset and its summary, the summary timestamped before the boundary as the runtime writes it |
| `replay: N` | the last N main-stream records re-emitted with their run rewritten |
| `system: {subtype, fields}` | a system record of any subtype |

A `tool` without a `result` is an unfinished tool. A `skill: {name, agent, steps}` is a fork whose
child is announced only in the parent's result. A `workflow: {name, children: [{name, prompt,
steps}]}` starts children as one batch, with a journal, a manifest and a script. The children run
at the same time, each started a moment after the one before, and the parent's next step waits for
the last of them. The runtime works the same way: in one Claude Code history measured in September
2026, the children overlapped in time in 165 of the 172 runs that had two or more children. Ids are
stable and the same in every format: the step's position names its records, a tool is `tool/<id>`,
a run is `<step>-cycle`, a child stream's id is derived from its name.

### Records the original lost

`lost: true` on a step says the original file does not hold what the step wrote: a person trimmed
the transcript, or the write never reached the disk. The step still happened. The clock and the
ids move as if the records were there, and the record after them still names them as its parent,
so the loss shows where it shows in a real corpus: in references the assembler cannot resolve. On
a `result` inside a `tool`, only the result is lost and the call stays. On an `agent`, a `skill` or
a workflow child, the child's file never reached the collector, nor its meta file, while the
parent's records about the child stay.

```yaml
steps:
  - call: {text: Reading it., tool: {name: Read, result: {text: "root: ./data", lost: true}}}
  - call: {text: Checking the port., tool: {name: Bash}}
    lost: true                                  # the call is gone; the result below is not
  - result: {of: s2-tool, text: "8787 LISTEN"}
  - call: {text: Asking the helper., agent: {name: helper, lost: true, steps: [{call: {text: Done.}}]}}
```

Both formats leave the same records out. The collector numbers the lines it finds, so the landed
data verifies and the chain is intact, and the document stays `verified`: nothing asz holds is
damaged, and the incompleteness is in the evidence. What a check sees is the `unresolved` list, a
`tool_result` for the first step, a `tool_use` for the second, a `child_stream` for the third.
`tests/scenarios/lost-records.yaml` covers every kind of loss.

## Check

```sh
asz scenario check FILE [--format claude-code|sd|all] [--out DIR] [--at TIME] [--scale FACTOR]
```

A checkpoint may also say what a person deleted from the storage root. `lose` names landed files
by what they hold, a stream or a run and a kind, since the two formats land the same files in a
different order. `nth` picks one file of that kind, counting from one in the order the files
landed, among those still there. The default is the first. The runner deletes them after the
checkpoint's parse, once a round has bound to them. Every check from there on runs over the damaged
root. The structure survives, because it lives in the rounds, and the text of the lost file is
gone. The document says `incomplete` and names the round and the sequence, and the session goes on
into later rounds.

`asz verify` reports the round and the sequence too. For a stream read line by line, such as a
transcript, it also reports the gap the lost file leaves in the stream. That is an `ord gap` and a
`byte gap` when the stream's first or a middle file goes, and an `end gap` when its last or only
file goes, since the stream's cursor says more was read. So `lost-file.yaml`, which loses a
helper's only transcript file, counts two problems where the document counts one.
`lost-ends.yaml` loses the first and then the last file of a stream. A `claude-code` build shows a
lost sidecar of a child agent, manifest or script only in the chain, because the adapter reads each
of them whole and tracks it by a digest, not a position. An `sd` build gives each of them an append
cursor that counts records, so there the same loss is an end gap as well. A checkpoint has one
`verify` count for both formats, so a scenario that loses such a file can pass in one format only.

Three properties cannot hold on such a root and are set off with a reason: a re-cut root holds
only what is on disk, a source line of the lost file has no landed record, and re-deriving from the
landed files changes the fold. `tests/scenarios/lost-file.yaml` is the example, and its loss
travels over the wire like anything else. The root rebuilt from the push reports every problem the
pushed root reports except an end gap, because a cursor stays in the storage root and never
travels.

For each format, and at each checkpoint in order, `check` builds through the checkpoint, collects
when the format needs it, parses, and compares the fold with the expectation file beside the
scenario, `NAME.expect.yaml`. At the end it runs the properties every chain must have, and with
`all`, the default, it compares the formats' folds with each other. It exits non-zero on any
failure and keeps its directory when one is given.

```yaml
checkpoints:
  built:                                  # named in the scenario
    rounds: 1
    kinds: {tool: 1, llm.call: 1}
    nodes:
      tool/t1: {refs: 2, attrs: {result: available, timing: unavailable}}
  final:                                  # the end of the scenario
    rounds: 2
    delta: true                           # the round written here is a delta
    talks_on: {main: 2, checker: 1}       # a stream may be named by its scenario name
    runs_in: {talk/main/s1-cycle: 2}
    relations: {starts: 1, reports: 1}
    unresolved: {open: 0, resolved: 0}
    unresolved_kinds: {tool_result: none}
    session: {from: +0s, to: +11.1s}      # the session node's range, as deltas from --at
    view: {state: verified, problems: 0, talks: 3, files: 6, first_talk: {label: run the build, runs: 2}}
    verify: {problems: 0}                 # what asz verify reports over the root
  helped:
    lose: [{stream: checker, kind: transcript}]   # deleted from the root after this checkpoint's parse
    view: {state: incomplete, problems: 1}
    verify: {problems: 2}                 # a round's missing file and the checker's end gap
properties:                               # all on unless set false
  reproducible: true
  fold_equals_parse: true
  immutable_rounds: true
  bundle: true
  header_matches_fold: true
  records_well_formed: true
  repack_keeps_structure: true
  recollect_idempotent: true              # runtime formats only
  every_line_a_record: true               # runtime formats only
  parts_keep_source_bytes: true           # runtime formats only
  discovery_ignores_noise: true           # runtime formats only
  pruned_sources_gone: true               # runtime formats only
  cross_format: true
  records_match: true
  push_follows_the_wire: true
  removed_after_sent: true
  view_covers_the_session: true
parse:
  max_round_bytes: 0                      # a parse setting, when the scenario needs one
push:
  kinds: [transcript, agent_meta, journal, workflow_manifest, workflow_script, round]   # kinds the push must carry
```

Only what is written is checked. The properties are: two parses of the same landed files write
the same rounds; folding every round equals one full parse; rounds verify, link and are not
writable; the landed files and rounds are self-sufficient without index and state; the head
round's header says what the fold holds; a parse with no new evidence writes nothing; every landed
record carries only the fields the format states a purpose for; a repack under the smallest budget
keeps every record and the whole structure; and, for a runtime format, a second collect lands
nothing, every source line becomes one landed record, parts carrying source JSON keep its bytes,
discovery passes over the noise the writer plants beside the session, and a pass after Claude Code
prunes the session's files sets their active cursors to `source_gone` and lands nothing. Across
formats, the folds must agree, and so must
the landed records themselves, field by field: the runtime's adapter and the sd writer must land
the same evidence from the same scenario, which is what makes a scenario a conformance test for an
adapter.

`parts_keep_source_bytes` compares each checked record's `sha` with the digest of the source bytes
named by `off` and `bytes`. A call, result or data part with `data` must hold bytes found unchanged in
that source record; an unknown part must return such bytes through `Part.Raw`. Media and change records
derived beside a result are skipped, because their encoding is the adapter's own. Only the newest
version of a rewritten source is checked, since that is what the source file still holds. This
property runs in `internal/scenario/run` for `claude-code` scenarios only. It does not check real
source files during collection. `source-bytes.yaml` exercises `<`, `>`, `&`, U+2028 and U+2029 in
source JSON and unknown script bytes.

The document is checked too: at the end of every scenario, `view_covers_the_session` holds the
`asz.view` document to the whole session: every round, verified; every landed file with its digest
as on disk; every talk, run and step of the fold in a tree; the session's own range; and a verified
state. A scenario with checkpoints is the multi-round case: `three-rounds` lands and parses in
three stages, from the start to the first checkpoint, from there to the second, and from there to
the end, so three rounds sit over landed files cut at each stage, and the final document must
cover the session as one parse would.

The push is checked too. Every scenario, in both formats, is pushed to a receiver in the test,
over gRPC and then over HTTP, one file per request, and every request is compared with the tables of
[Export over OpenTelemetry](../setup/export-otlp.md): the resource and the scope, one record per
file with the file's bytes and digest, the attributes a landed file carries and the ones only a
round carries, the record time range and the list attributes, and the stamp a receiver bounds a
read on. A refused request must leave every file for the next pass, a second pass must send
nothing, and writing every body back to its path must give a root that verifies and folds the
same. `push.kinds` names the file kinds a scenario's push must carry; `all-kinds.yaml` names all
six.

The removal is checked too. `removed_after_sent` makes full copies of the finished root and runs a
pipeline's pass over each: collect, derive, parse, send to a receiver in the test, then remove.
Nothing is removed before a push, after a refused push, after a partial success, when `push.state`
names another receiver, or when the marker is gone. After a clean push, `immediately` removes the
whole session in one pass, and after that nothing is landed, derived, parsed or sent again. A
retention of 24 hours keeps the session one second before its last record is 24 hours old, and
removes it at that moment. An sd build is never marked. `removed-after-sent.yaml` holds every kind
of file a removal deletes, and the tests in `tests/chain` stop its removal at every point it
reaches.

Pruning is checked too. `pruned_sources_gone` works on a copy of the finished root. It deletes the
session's main transcript, then every other file of the session that the Claude Code adapter finds,
and then puts them all back with the bytes they had. When the session has other files, the first
step leaves it for discovery to find and the second does not, so both ways a pass finds a pruned
file's cursor are checked. After each step, one pass of both adapters runs, as a newly started
process would. A deleted file's active cursor must then say `source_gone`, and every other cursor
must say what it said before, with no cursor added or removed. A cursor that already said
`conflict` stays that way. Once the files are back, every cursor must say again what it said before.
After every step the pass must land nothing and meet no new conflict or error, every landed file
must keep its digest, a parse must write no round, and `asz verify` must count as many problems as
it counted before.

The project's own tests are scenarios under `tests/scenarios/`, one property of assembly each,
run in both formats by `go test ./tests/`.
