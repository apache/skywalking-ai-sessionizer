# Claude Code Adapter

How the [Unified Conversation Model](../concepts-and-designs/unified-conversation-model.md) maps onto
Claude Code. Every mapping below is stated with the evidence that supports it and, where a concept
cannot be supplied, said to be unavailable rather than approximated.

**Verified against** Claude Code 2.1.220 – 2.1.251. No earlier version was available for measurement,
so nothing here is confirmed below 2.1.220.

**Measured on** one development machine's Claude Code history. It was live and grew while it was
measured: about 2,920 files and 1.2 GB in the first pass, 2,970 files, 1.09 GB and 365,825 records
when it was measured again, and 3,032 files in the last pass. A count is a snapshot. Where two
passes disagree, or a count covers only part of the corpus, the page says which.

## Collection

The adapter reads local files. It requires **no configuration, no environment variables, and no
cooperation from Claude Code** — and because it reads what is already on disk, it works on
conversation history that predates its installation.

```text
~/.claude/projects/<slugified-cwd>/<session-id>.jsonl                       main stream
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.meta.json
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/agent-<agent-id>.meta.json
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/journal.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/wf_<run-id>.json
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/scripts/<label>-wf_<run-id>.js
```

A workflow script carries its run id in the filename, which is what makes it collectable
despite living outside its session's directory. Split on the **last** `-wf_`: a run id itself
contains hyphens (`wf_afa31e47-f6c`), so a greedy match starts at the first occurrence and
swallows any later one. On a real corpus, 172 of 172 script names parsed this way, and all 172 run
ids matched a manifest in their own session.

Transcripts are the primary channel because their content is complete. They store the person's
prompt text, the assistant's prose, tool inputs and tool results in full. Claude Code's
OpenTelemetry events do not. By default they redact `user_prompt` and `assistant_response`, cut
content at about 60 KB, and omit tool inputs unless `OTEL_LOG_TOOL_DETAILS` is set. Those three
defaults were read from the 2.1.245 binary. Events also redact reasoning in every case. Transcripts
have two exceptions of their own. Reasoning text is usually empty, see
[What this adapter cannot supply](#what-this-adapter-cannot-supply). And on the main stream, a long
tool result's content block can be shorter than `toolUseResult.stdout`, see
[Session Data mapping](#session-data-mapping).

OpenTelemetry is an optional second channel that adds measurement only — see
[OTLP](#otlp-optional-second-channel).

### Discovery is session-first

A directory under `projects/` is named after the working directory Claude Code was launched in, with
`/`, `.` and space all collapsed to `-`. That transformation is **not reversible** — the real path is
recoverable only from record content. The rule was checked across all 25 source directories that
held a transcript. Two did not fit a plain `/` rule, and they showed that `.` and space are replaced
too: `/Users/…/Application Support/JetBrains/GoLand2026.1/…` became
`-Users-…-Application-Support-JetBrains-GoLand2026-1-…`, and
`/private/tmp/agentsessionizer-cc-v04.psixvf` became `-private-tmp-agentsessionizer-cc-v04-psixvf`.
A [session filter](../setup/configuration.md#session-filters) that starts with `/` is converted
forward the same way. Because the conversion loses information, `/private/tmp/**` matches the
directory itself and any name that begins with its converted form followed by `-`.

More importantly, **a session's files are not confined to one such directory**. Claude Code files a
workflow run's *script* under whatever working directory the agent had at the time. Measured on a
real corpus, 7 of 61 sessions span more than one directory for this reason, one of them across six.
The conversation data itself - main transcript, child streams, journals, manifests - stays together;
it is the script that travels.

Discovery therefore scans every directory for entries whose name is a session id — both `.jsonl`
files and directories — and **groups by session id across directories**. A directory-first walk
would attribute a session's scripts to a directory that holds nothing else of it.

A directory is recorded as belonging to a session only once it has yielded a collectable source.
Otherwise a directory containing nothing we collect could disqualify the whole session through an
exclude pattern — which is why filtering keys on the session's *primary* directory, the one
holding its main transcript.

Child files are found at exactly two levels under a session directory: `subagents/` and
`subagents/workflows/<run-id>/`. Nothing deeper is read, so a child file placed deeper would be
missed. The second level is required. In the 2,970-file pass, a listing of `subagents/` alone found
107 child files where the full walk found 2,910, a 96.3% miss, because most children live under
`subagents/workflows/<run-id>/`.

Every pass discovers the sources again rather than keeping a list, so a file that appears after one
pass is collected by the next. A project directory that vanishes during a scan is skipped, because
the corpus is live. One that cannot be read for any other reason is an error for the pass, so a
pass that could not see every source does not report itself complete. That holds for project
directories only. Inside a session, a directory that cannot be read is skipped with no error: the
session directory itself, `subagents/`, `subagents/workflows/`, a run directory under it,
`workflows/` and `workflows/scripts/`. The pass then reports itself complete without the files
under it.

Two further rules:

- `memory/` is a sibling directory inside a project directory and is **not** a session. Match session
  directories by identifier shape, not by "is a directory".
- **Pruning is real and not atomic.** Claude Code deletes transcripts. Of 345 session ids in its
  prompt history, `history.jsonl`, 330 had no surviving transcript. In the first pass, 12 of 41
  session directories had no main transcript while their whole `subagents/` tree survived. That is a
  normal state, not corruption. When a source disappears, the files already landed stay, so the
  storage root outlives the source. A later pass sets its cursor to `source_gone`, whether
  discovery still finds the session or not, except in the cases
  [Pruned sources](../formats/storage-root.md#pruned-sources) describes. Separately, 32 of 61 main
  transcripts had no subagent directory at all, which is a session that started no agent.

### How each source is read

| Source | How it changes | How it is read |
| --- | --- | --- |
| `<session-id>.jsonl`, `agent-<agent-id>.jsonl` | only grows | from a byte offset: each pass lands what was appended since the last one |
| `journal.jsonl` | only grows | from a byte offset |
| `wf_<run-id>.json` | rewritten as the run progresses | by content digest: a changed file lands again as a new version |
| `<label>-wf_<run-id>.js` | may be rewritten | by content digest |
| `agent-<agent-id>.meta.json` | written once, at spawn | by content digest, so in practice it lands once |

Reading by offset is safe because Claude Code only appends to a transcript. Resuming a session
appends to the same file, see [Conversation identity](#conversation-identity), and the block
Claude Code re-writes before a context reset is also an append, see
[Reconstruction rules](#reconstruction-rules). Across 60 main and 2,851 subagent files in the first
pass, no parent id dangled and none pointed into another file. Truncation was never observed, but
that is not proven. A cursor's size check and its tail digest catch it, and the collector then stops
that source with a conflict rather than read from a position that no longer means what it did.

A session is never finished. A transcript quiet for weeks can grow again when the session is
resumed, so the adapter never marks a session done, and every pass checks every source again.
Collection polls, as [collector](../setup/configuration.md#collector) describes. There is no file
system watcher.

Each child lands flat by its agent id, beside `main`, as the [storage root](../formats/storage-root.md)
describes. That is safe because no agent id appears in two files, see
[Execution streams](#execution-streams). Nesting children under their run or their parent would
write a derived join into a write-once path. A join that later proved wrong would then mean moving
files, and that matters most for the 31 depth-2 agents. Only a name of the shape
`^a[0-9a-f]{16}$` counts as an agent id. If two source files would still land in one stream or run
directory, the collector refuses the second and counts a conflict, rather than mix two files behind
one cursor.

### What is not read

The adapter reads the conversation records and nothing else. The project's subject is conversation
structure and provenance. The files below hold the runtime's working state or output spilled from a
tool, not conversation records, so they are not read:

- elsewhere under `~/.claude/` or beside it: the prompt history `history.jsonl`, `~/.claude.json`,
  `file-history/`, the telemetry spool `telemetry/1p_failed_events.*`, the live process registry
  `sessions/<pid>.json`, `shell-snapshots/`, `session-env/`, `backups/` and `paste-cache/`;
- inside a project directory: `memory/`, `tool-results/` and `sessions-index.json`.

Which files a tool call changed comes from the transcript's own patch and from the plugin, see
[Workspace changes](#workspace-changes), never from `file-history/`. The prompt history appears on
this page only as a measurement, under [Discovery is session-first](#discovery-is-session-first).

## Object mapping

| Model object | Claude Code | Notes |
| --- | --- | --- |
| **Conversation** | `sessionId` | the only mapping implemented. See [Conversation identity](#conversation-identity). |
| **Segment** | derived | an activity window chosen by the assembler, not a runtime concept |
| **Session** | `sessionId` | 1:1 with Conversation |
| **ExecutionStream** `main` | `<session-id>.jsonl` | the only stream that compacts |
| **ExecutionStream** child | `agent-<agent-id>.jsonl` | named by the agent id in its file name. It shares the parent `sessionId`. |
| **Context epoch** | split by `system/compact_boundary` | main stream only |
| **Talk** | derived from the trigger of each prompt cycle | not a runtime concept. See [Talks and runs](#talks-and-runs). |
| **Run** | `promptId`, within one stream | one run per distinct `promptId` in a stream |

### Conversation identity

`sessionId` is the default conversation identity because **nothing on disk reliably links two
sessions**. `bridgeSessionId` spans two session ids in only 1 of 20 observed cases and can be the
empty string; the snake_case `session_id` field is entrypoint-gated and its apparent cross-session
links trace to a single stale value. No heuristic over `cwd`, `gitBranch`, `slug` or title is sound.

`/clear` starts a new `sessionId` and a new file, and is detectable at the **head of the successor
file** as a fixed three-record chain: a root `user` record with `isMeta:true` carrying
`<local-command-caveat>`, then a `user` record whose content is `<command-name>/clear</command-name>`,
then `system/local_command`. The successor carries no pointer back to its predecessor.

Resuming a session does not start a new one. Claude Code appends to the same file under the same
`sessionId`, so a resumed session continues the same session and the same conversation. One
transcript in the first pass held a 659-hour gap, across which `version` went from 2.1.220 to
2.1.251, and its parent chain still linked across the gap.

The session id is the only mapping implemented. `asz parse` always passes it as the conversation
id, and no flag, configuration key or API takes another one today. So an application that considers
several `/clear`-separated sessions one piece of work cannot yet say so. Supplying an explicit
conversation id is planned.

### Execution streams

A child agent **always shares the parent's `sessionId`** — measured across 2,697 subagent files with
zero exceptions and zero files carrying more than one session id. It never gets a session of its own.
The partition is clean: no sidechain records in main transcripts, no top-level `agentId` in main
transcripts.

**Stream membership comes from the file a record was written in, never from a field.** A child's
stream id is the agent id in its file name, and the collector writes it into the landed header. The
in-record `agentId` is not used to name a stream. That is safe. In a full scan of 2,686 agent files
and 218,393 records in the first pass, the file name's agent id equalled the in-record `agentId` in
2,686 of 2,686 files, no file carried more than one, and no two files shared one.

`isSidechain` looks like the discriminator and is not. It is missing on 28.7% of main-transcript
records in the 2,970-file pass, and inside a subagent file it is constant, so it says nothing about
any one record. asz does not read it.

| | main | child |
| --- | --- | --- |
| `sessionId` | S | **S — identical** |
| `isSidechain` | `false`, or missing (28.7% of records) | `true` |
| top-level `agentId` | absent | `^a[0-9a-f]{16}$` |

Nesting stops at depth 2. In the 2,970-file pass, 2,683 sidecars were at depth 1, 31 at depth 2,
and one had no depth field. No depth of 3 or more was seen. All 31 depth-2 agents come from one
session, so a method that samples a few sessions will likely see none and conclude that the maximum
is 1. The file layout carries no depth: every agent transcript sits flat in `subagents/` or in
`subagents/workflows/<run-id>/`. Nesting is known only from a nested child's sidecar
`parentAgentId`. asz does not read `spawnDepth`.

Child streams are structurally simpler than main: exactly one root per file, no duplicate record
ids, **no compaction at all** (0 boundaries in 221,592 child records in the 2,970-file pass), and no
rewind forks. Their model context accumulates without interruption for the life of the stream, so
each child stream is one context epoch.

### Context epochs

`system/compact_boundary` is an explicit, machine-readable epoch boundary — the model never has to
infer one. asz detects it by the system record's `subtype`, never by the text `compact_boundary`,
which appears far more often in message content than in real boundary records. An epoch is cut only
at such a record, in landed order, and a stream with no boundary is one epoch.

The boundary carries `compactMetadata` plus **`logicalParentUuid`**, a pointer to the last
pre-compaction message. In the 2,970-file pass it was present on 27 of 27 boundaries and on no other
record type. The boundary starts a new parent chain. Its own `parentUuid` is null on 27 of 27,
so `logicalParentUuid` is the only link back, and a parent walk across a reset stops without it.

- `logicalParentUuid` is never the line just before the boundary, so proximity cannot replace it.
- The boundary's `uuid` is the `parentUuid` of the immediately following `isCompactSummary:true`
  record, without exception. asz pairs the two through that parent.
- The summary's timestamp is **always earlier** than its own boundary's, by 1 ms to 1,097 ms on
  26 of 26 boundaries in the first pass. Never order an epoch boundary by timestamp.
- The summary carries a `promptId` and no `origin`. When no other record of its cycle states a
  trigger, that cycle starts a Talk, under the rule in [Talks and runs](#talks-and-runs).
- asz reads only `logicalParentUuid`. When it names a record that is not in the landed data, the
  round carries an open `epoch_continuation` reference. `compactMetadata.preservedMessages` is not
  read. One pointer is enough: `logicalParentUuid` equals the last entry of
  `preservedMessages.allUuids` on 26 of 26 boundaries in the first pass. `allUuids` can name records
  that exist nowhere on disk, 5 in that pass, which is why the pointer is checked rather than
  trusted.

### Talks and runs

Claude Code records neither a Talk nor a Run. Both are built from two of its fields, `promptId` and
`origin`, by an exact rule.

- **A run is one prompt cycle**: one per distinct `promptId` within a stream. A `promptId` is not
  unique across streams, because a child stream is written under a cycle id that also appears in its
  parent, so the key is the stream and the `promptId` together. A model response carries no
  `promptId`, and reaches one through its parents, as the
  [pipeline](../concepts-and-designs/conversation-assembly.md#the-pipeline) describes. On this
  machine on 2026-09-11, none of 192,743 assistant records carried a `promptId`. In a sample of 400
  assistant records, whose pass was not recorded, all 400 reached a `promptId` through
  `parentUuid`, in 1 to 7 hops, with a median of 2.
- **A cycle's trigger is `origin.kind`**, read from the record and then from `attachment.origin`.
  Reading only the record loses 27.6% of all trigger markers. `human` is external and
  `task-notification` is a notification. The value is an open set: a third kind exists, so any
  other value maps to `unknown`, and a switch on exactly two values would meet something it does
  not expect.
- **On the main stream, a Talk starts when a cycle has an external trigger, or when no record of
  the cycle states any trigger.** A local slash command writes no `origin`, and it is a person
  acting, so a rule that required an explicit marker would attach it to whatever came before. A
  cycle whose only stated triggers are notifications or an unknown kind continues the Talk in
  progress. A child stream is one Talk, created up front, and each of its cycles is a run in it.

The count behind the rule: distinct `promptId` values equal human cycles, plus notification
cycles, plus cycles with no `origin`. One sampled session had 65 cycles, 36 human and 29
notification. On one real 59,495-record session, the main stream assembles to 307 Talks from 336
cycles, which equals the number of `origin.kind:"human"` records counted independently on the raw
file.

The count holds only after duplicate records are removed. A re-emitted copy rewrites `origin`, so a
Talk splitter that runs before removal over-counts human triggers by up to 15% in the 2,970-file
pass.

## Step mapping

| Model kind | Claude Code source | Quality |
| --- | --- | --- |
| `message.external` | `user` record with `origin.kind:"human"` and without `isMeta`; a `queued_command` attachment with `commandMode:"prompt"` | `exact_unique` |
| `message.assistant` | assistant `text` block | `exact_unique` |
| `message.synthetic` | `message.model == "<synthetic>"` | `exact_unique` |
| `context.injection` | every other `attachment` record; a `user` record with `isMeta:true` | `exact_unique` |
| `llm.call` | assistant records sharing one `message.id` in the session | `exact_unique` |
| `thinking` | assistant `thinking` or `redacted_thinking` block | `exact_unique`. The text is usually `unavailable`. |
| `tool` | assistant `tool_use` block joined to its `tool_result` block; enriched by `toolUseResult` | `exact_unique` |
| `agent.call` | a `tool_use` block that a child stream is joined to | `exact_unique` |
| `agent.launch_ack` | `toolUseResult.status:"async_launched"` | `exact_unique` |
| `agent.output` | the child stream's last non-synthetic response, plus the run journal's result record when there is one | `exact_unique` |
| `runtime.notification` | a record whose `origin.kind` is `task-notification`, on the record or on `attachment.origin` | `exact_unique` |
| `epoch.boundary` | `system/compact_boundary` | `exact_unique` |
| `epoch.summary` | following `isCompactSummary:true` record | `exact_unique` |
| `error.api` | `system/api_error` | `exact_unique`. The retry state is `unavailable`. |
| `control.interrupt` | `[Request interrupted by user]` | `unavailable`: not emitted today |
| `control.permission` | denial strings, `permission-mode` records | `unavailable`: not emitted today |
| `control.command` | `system/local_command` | `exact_unique` |
| `turn.duration` | `system/turn_duration` | `exact_unique` |

**Every attachment is injected context by default.** There is no list of types, because a list has
to be right about types that do not exist yet. An earlier list was wrong in both directions: 635
records across 13 real attachment types fell through it and were dropped, while 5 types on it
appear nowhere in the corpus. The first pass counted about 21,000 attachment records of 32 types. A
`user` record with `isMeta:true` is text the harness wrote in the user role, so it is injected
context too, not something a person said.

**One attachment is a person's input.** A `queued_command` with `commandMode:"prompt"` is a message
typed while the agent was working. It becomes `message.external`, and any other mode stays
`context.injection`. These turns have no `.message` twin. In the first pass, 614 human-typed turns
existed only as `queued_command` records, and in the 2,970-file pass a third of all human
utterances lived only there, so a reader that follows messages alone loses them. The mode is needed,
not only the type: of 1,558 unique queued records in the first pass, only 960 were human input, and
reading the type alone would insert 598 machine-generated `<task-notification>` blobs as things the
person typed. [asz.view](../formats/asz-view.md) takes such a turn's text from the envelope's
`prompt[].text`. Most of these turns do not start a Talk. In the 2,970-file pass, 886 of 918
joined a cycle that was already running. Such a record is placed in the enclosing Talk
through its parents, and asz invents no cycle id for it. The scenario
`tests/scenarios/attachment-input.yaml` checks both modes.

One tool use is one step. The `tool_use` block supplies `name` and `input`. For `Bash`, the input
is the command itself. The `tool_result` block supplies the result. The join key is the exact
tool-use id, looked up across every stream of the session, and nothing matches on a prefix. Once
duplicate records are removed the join is one-to-one. Across 2,924 files, 356,583 records and
207,984 tool blocks in the first pass, each of 103,503 keys had exactly one result, and no key was
shared across files. Several results for one id would make the step `exact_ambiguous`, with an open
`tool_result` reference. A provider call to its tools is one-to-many, up to 16.

The 2,970-file pass did find tool uses with no result, on child streams only: 7 of 79,418 tool uses
there, each on the last line of its file. On the main stream it was 0 of 25,892. Such a step
carries `result: unavailable` and an open `tool_result` reference.

A second join exists in the data and is not used. Every tool result carries a top-level
`sourceToolAssistantUUID`, and it matched the record holding the corresponding `tool_use` in 105,409
of 105,409 cases. asz does not read it and does not run that check.

A tool use becomes `agent.call` only when the spawn join finds a child stream that came out of it.
The kind comes from that join, not from the tool's name. A tool use whose child never landed stays
`tool`, and the round carries an open reference, see [Agent spawn](#agent-spawn). The tool that
starts an agent is named `Agent`. `Task` appears nowhere as a tool name in the corpus.

`toolUseResult` enriches the result with structure the content block lacks — `stdout`/`stderr` split,
`structuredPatch`, interruption flags — but almost only on the **main** stream: present on 100% of
main-transcript tool results and 2.1% of child-stream ones. Everything read from it, including the
spawn joins below, is therefore a main-stream mechanism. How it lands is in
[Session Data mapping](#session-data-mapping).

**`toolUseResult` is not always an object.** On roughly 7% of results it is a bare string. Decoding it
into a struct fails, and because the failure covers the whole record, every identifier on that record
is lost with it — the tool join, the prompt cycle, the parent link. It must be read as raw JSON and
shape-checked before any field access. asz reads it raw and uses it only when it is an object.

`timing` is `unavailable` on every tool step. Claude Code writes no separate execution record
locally, and inferring a duration from the gap between the request and result records would report
queueing and model latency as tool time. Three tools do report a duration the runtime measured,
`WebFetch.durationMs`, `WebSearch.durationSeconds` and `Agent.totalDurationMs`. asz does not read
them today, so those three are `unavailable` too. A configured timeout is not a measurement.
[asz.view](../formats/asz-view.md#a-node-in-talks) reports the time from the request record to the
result record where the join is exact. That is the interval between two records, not execution
time. OTLP's `claude_code.tool.execution` span supplies the rest, which is Phase 2.

**Absence of `is_error` does not mean success.** It is present on 85.5% of result blocks. asz sets
`failed` only when the block carries it.

`error.api` comes from `system/api_error` alone. asz reads no retry field, so every such step
carries `retry_state: unavailable`, and no `retries` relation is written.

`control.interrupt` and `control.permission` are not emitted today. An interrupt does not break the
tool join, because the `[Request interrupted by user]` record is written after the tool result and
the join does not depend on it. The next turn can re-parent to an earlier record, and it is still
placed in its run through its own parents. No `cancels` relation is written, and the re-parent is
not recorded as a stream discontinuity. The generated [glossary](claude-code-glossary.md) keeps the
runtime's name for both kinds and for `cancels`, because the runtime has a word for each. Its note
says asz does not read it yet.

### Provider calls

**Group by `message.id` across the whole session. Never group by `requestId`.** `message.id` was
present on 115,162 of 115,162 assistant records in the first pass, and it never appeared in more
than one file, so the fragments of one call all sit in one stream. `requestId` is absent on some genuine
calls, and a `<synthetic>` companion record can reuse a real call's `requestId`, which would put
fabricated content inside a real provider call. A test on `requestId` is wrong in both directions:
in the first pass, 24 of 97 synthetic records carried a non-null `requestId`, and 2 genuine calls
carried none. The synthetic test is `message.model == "<synthetic>"`. A synthetic record can never
join a real call: in the 2,970-file pass, all 122 synthetic records had a UUID-shaped message id,
and all 184,330 real ones start with `msg_`.

A single response is split across several JSONL lines — 2.09 fragments per call corpus-wide, ranging
1.59 to 2.51 per session. **Line order is authoritative. Timestamps are not.** Fragments are sorted
by landed file and row, which reproduces source order. In the first pass, 372 provider calls had
fragments with tied timestamps, and at file level 2.76% of timestamped records were out of order
after duplicates were removed. A call's fragments are **not contiguous**: on 24% of multi-fragment
calls their own tool results sit between them, so grouping must scan by key rather than read a run
of lines.

**Usage: take the last fragment in LINE ORDER. Never sum, and never use `stop_reason` to find it.**

`stop_reason` is not a terminal marker. A main transcript stamps the same value on every fragment of a
call, in 23,324 of 23,329 calls in the 2,970-file pass. A subagent transcript stamps it only on the
last. So a `stop_reason` test picks the *first* fragment on main and the last on a child. Only line
order works on both.

Summing is wrong for a different reason on each side. Main repeats the final usage on every fragment,
so adding them multiplies the real number by the fragment count — 2.18× pooled, 9× worst case.
Subagent partials climb `1, 2, 3 …`, where the last value is the whole count, and summing them adds
only 2%.

About 10% of calls never report a `stop_reason` (0.02% main, 14.5% subagent, in the 2,970-file
pass). Two consequences:

- Their usage block is **present and wrong**, a streaming stub. 5,739 of 7,410 such calls have a last
  output count of 5 or less, against 101 of 66,751 finished ones. Usage availability is gated on
  `stop_reason`, never on the field being present.
- They are **not in flight**. Only 72 of 7,410 sit at end-of-file; the rest are mid-file with the
  conversation continuing past them, and 7,311 of them had their tools run. Nothing waits for them,
  and nothing is marked open.

Every `llm.call` node in a round carries `fragments` and `usage_from: last_fragment_in_line_order`. A
finished call adds `usage: observed_replayable` and `usage_at`, the `{seq, row}` of the fragment the
usage is read from. An unfinished call has `usage: unavailable` and `stop_reason: unavailable`.
[asz.view](../formats/asz-view.md#a-node-in-talks) copies the token counts from the one record
`usage_at` names.

### Agent spawn

No single field spans the mechanisms, and they do very unequal amounts of work. The share was
measured in the 2,970-file pass.

| Mechanism | Parent → child join | Quality | Share |
| --- | --- | --- | --- |
| **Workflow** | the `Workflow` result's `runId`, plus `journal.jsonl` `{type:"started", agentId}` naming each child | `strong_inference` | ~96% |
| **Agent tool** | the parent's `toolUseResult.agentId` | `exact_unique` | |
| **Skill fork** | the same field with `status:"forked"`; no separate pointer to the call, so the tool id comes from the result's own content block | `exact_unique` | |
| **Nesting** | the child sidecar's `parentAgentId` | no `starts` relation, see below | 31 children |
| | *together* | | ~4% |

The quality is on the `starts` relation from the call to the child stream. A journal edge is
`strong_inference` because the journal names no call. The call comes from the launch result that
named the run: asz resolves the run to the tool use whose result names it, so the edge runs from
that call to the child, with no node standing for the run. An edge from a parent result or from a
completion notification is `exact_unique`.

**Workflow membership comes from the journal, never from `agentType`.** A workflow step can ask for a
named agent type, so `agentType` does not identify a workflow child, and asz does not read it.
Discovery finds a workflow child under `subagents/workflows/<run-id>/`, and the run's journal names
it. In the first pass, each `journal.jsonl` `{type:"started", agentId}` matched a child file, 2,583 of
2,583.

**`agent-<id>.meta.json` is not a general parent pointer.** It is written once, at spawn: 200 of 200
sampled sidecars were older than their own transcript, none equal and none newer. Its content is
spawn-time only, `agentType`, `description`, `spawnDepth`, `toolUseId`, `parentAgentId` and `model`,
and never a status, a result or a duration. Most sidecars hold only `agentType` and `spawnDepth`.
On this machine on 2026-09-11 that was 2,534 of 2,748 sidecars, and 2,517 of them belonged to
workflow children. So most children carry no pointer to their parent in their own sidecar. There is
no registry file. Each agent owns its sidecar. Pairing was exact: 2,697 sidecars to 2,697
transcripts, with no orphan either way.

Beyond which child it belongs to, asz reads two fields from a sidecar. `description` becomes the
child stream's label. `parentAgentId` names the agent that started a nested child, and is the only
evidence of nesting anywhere. asz reads no call id from a sidecar, so a sidecar never anchors a spawn
on a call.

A nested child named only by its sidecar is counted as joined and is not reported as an orphan. But
no `starts` relation is written for it, because that edge has no call to start from. Like every
child stream it sits under the session. A nested child whose completion notification names both the
call and the child's agent id gets its `starts` relation through the notification.

**The journal answers one question:** which children belong to a run. It carries no timestamp, no
name, no parent pointer and no tool id. The launch itself comes from the parent's result, which names
the run. 10.24% of workflow children never get a terminal journal record, so "started with no result"
is normal.

**`toolUseResult.status` has three values.** `async_launched` is a launch acknowledgement and not a
result. `forked` is a skill fork. `completed` is a **synchronous return that IS the result** and
carries `agentId`.

**The child owns its output.** The child's messages and tools stay in the child stream. `agent.output`
is a node in the child stream, with an `ends_with` relation from the stream to it. It points at the
child's last non-synthetic response, and at the run journal's result record when there is one, and
`returned_value` is then `observed_replayable`, otherwise `unavailable`. The journal's result is what
the child returned, and it is not in the child's transcript: 2,353 of 2,436 journal results appear
nowhere else, on a snapshot of the development corpus whose size was not recorded. asz does not drop
the parent's copy. A synchronous return, `status:"completed"`, still appears as the result of the
parent's `agent.call`.

Completion arrives as a `<task-notification>`, and its two ids are not equally good. asz reads
them only from a record whose message content is a plain string. The notification step comes from
the record's trigger, not from this text, see [Step mapping](#step-mapping).
**`<task-id>` is a generic task handle.** It is an `agentId` only about 8% of the time, and
otherwise a background-command id, a workflow task id or a monitor id. Joining on it
unconditionally matches the wrong thing most of the time. `<tool-use-id>` is exact, but it names
the call, not the child, so asz tries the task id first. A single notification can carry several
`<task-id>` elements. asz takes the first one that has an agent id's shape, and
links the notification to that child with a `reports` relation, `exact_unique`. When that child's
transcript has not landed, the round carries an open `notified_child` reference instead. With no
such task id, asz resolves through the call named by `<tool-use-id>`, whose spawn edge names the
child, and the `reports` relation is `strong_inference`.

Two cautions. The `<tool-use-id>` is **not always resolvable in the file it lands in** — a nested
child notifies the main session while the call that started it lives in an agent file — so resolution
searches the session's whole file set. And `<status>` is **absent entirely** on a large class of
notifications, so asz does not read it.

The workflow journal **leads the filesystem**: a `started`, even a `result`, can be journalled before
the child transcript exists. That is normal for a live session, not corruption. Once the run is over
it is instead evidence of a pruned or lost file. Until the file appears, the round carries an open
`child_stream` reference, and a later round resolves it. A batch whose launch call was never landed
is carried as an open `spawn_call` reference.

**A child with no parent at all exists.** In the 2,970-file pass, 99.93% of children resolved and two
did not. An orphan stream stays under the session and in no parent's Talk, and the round carries an
open `spawn_of_child` reference for it, because guessing a parent is worse than saying none was
found.

## Reconstruction rules

Three rules, each required for correctness rather than efficiency.

**1. Remove duplicate records by record id, across the whole session, before anything else.** Record
ids are not unique within a file. In the 2,970-file pass, 1,533 ids repeated among 318,199, in 11 of
62 main transcripts and **0 of 2,908 subagent transcripts**, always with exactly two copies, so one
pass is enough. Every repeat measured was inside one file. No measurement shows an id repeated across
two files, and keying on the id across every file of the session relies on that. A record with no id
is never treated as a copy. The same rule removes a record landed twice when a collector pass is
interrupted between landing a file and saving its cursor. The scenario `tests/scenarios/replay.yaml`
checks it.

Keep the **first** occurrence, and take nothing from the later one. The later copy is the worse one:
its `promptId` is rewritten on 538 of 1,533 pairs, its `parentUuid` differs on 40, and where both carry
`toolUseResult` the first is larger on 430 pairs and smaller on **zero** — 43.4% of captured tool
output would be discarded, with `stdout` blanked to `""` on 314 pairs. The `message` object itself is
byte-identical on every pair, which is why this looks harmless and is not. The timestamps of the
two copies are identical on 1,533 of 1,533 pairs in the 2,970-file pass, so time cannot say which
copy came first. Nothing distinguishes an original from a copy except line order. The copies stay
in the index as evidence and are counted, and every lookup leaves them out.

Copies also make the tool join look ambiguous when it is not. In the first pass, before removal, 441
tool-use ids joined ambiguously, 882 of 64,722 tool results (1.4%), only because of the re-emitted
copies.

These duplicate blocks are not noise. The first pass found 1,530 duplicated records in 10 main
transcripts, and measured all 10. Each block is a re-emission just before a context reset: all 10
end on the line immediately before a `compact_boundary`, and only 2 of 10 are a dense, contiguous
replay of a prefix. The block is the set of records lying off the following boundary's ancestry. In
10 of 10 files no duplicate lay outside that set, and in 8 of 10 no record off the ancestry was left
out of the block. 0 of 1,530 duplicated records lie in the ancestry of the following boundary's
`logicalParentUuid`. In the two exceptions, 330 and 37 records off the chain were not re-emitted,
and they cluster in whole abandoned runs. That establishes **context = chain ancestry ∪ off-chain
siblings**, and distinguishes parallel-tool siblings, which are re-emitted and in context, from
abandoned branches, which are dropped.

**2. Group a provider call by `message.id`, in line order. Never rebuild a call or a message list
from `parentUuid` links.** The `parentUuid` graph is a **DAG, not a chain**, and two walks over it fail
for two different reasons.

- **Walking backward from a call to rebuild its message list** misses the sibling branch at a fork.
  Parallel tool dispatch forks the graph, `tool_use#1 → {tool_use#2 (same message.id),
  tool_result#1}`, so an ancestor walk traverses one branch and drops the other `tool_use` and its
  result. In the first pass, 33,798 of 86,687 provider calls (39%) rebuilt this way carried a
  `tool_use` with no matching `tool_result`, a body the Messages API would reject. Line-order
  reconstruction was balanced in 2,622 of 2,623 tool-bearing files.
- **Walking forward from a call's first fragment to collect its fragments** drops a tool use because
  a call's fragments are interleaved with their own tool results, see
  [Provider calls](#provider-calls). It is not a fork: no fragment has two children sharing its
  message id. In the 2,970-file pass, the forward walk dropped a `tool_use` on 16,063 of 85,057
  tool-bearing calls: 18.9% pooled, 2.1% on the main stream and 25.9% on child streams.

Forks are more common where most calls are. The first pass found 4,073 forks, and every fork had
exactly two children: a `tool_use` and a `tool_result` in 3,905, an assistant record and an
`api_error` in 160. The fork rate was 0.49% on main and 1.69% on child streams, and child streams
hold about 69% of provider calls, so a measurement on main transcripts alone understates the
problem. Message-id grouping does not depend on the parent links at all: fragments of one message id
never interleave with those of another, 0 of 86,716 calls.

The ban is on rebuilding, not on the edge. asz does walk parent links backward, at most 64 hops, to
find the run a record belongs to. That walk is single-valued, because a record has exactly one
containment parent.

**3. Always iterate the full `content` array.** A line usually carries one block but sometimes carries
several, mostly on `user` records. On this machine on 2026-09-11, 198 records in 2,832 transcript
files carried more than one block: 186 `user` records and 12 `assistant` records. 55 of them were
`[image, text]`, where reading `content[0]` renders a base64 image as the user's prompt. On 9 of the
12 assistant records a `tool_use` block came after the first block, so reading only the first would
break the tool join.

## Session Data mapping

The adapter converts each record once, while it is landed, into [Session Data](../formats/session-data.md).
This section says what each Claude Code shape becomes. The roles a record plays are in the generated
[glossary](claude-code-glossary.md).

Every content block in the 3,032-file pass, 1.1 GB:

| Claude Code block | Keys | Count | Part | State |
| --- | --- | ---: | --- | --- |
| `tool_result` | `content` `is_error` `tool_use_id` | 108,243 | `result` | `available` |
| `tool_use` | `id` `name` `input` `caller` | 108,149 | `call`, the input in `data` | `available` |
| `thinking` | `thinking` `signature` | 60,382 | `reasoning` | `available` with text, `unavailable` when empty |
| `text` | `text` | 22,458 | `text` | `available` |
| `image` | `source{data, media_type}` | 41 | `media`, the base64 inline in `data` | `available` |
| `fallback` | `from{model}` `to{model}` | 1 | `unknown`, the bytes kept | `available` |

Four of the six cover 99.99% of content blocks. Two more shapes are converted, though that pass
counted none: `redacted_thinking` becomes `reasoning` marked `redacted`, and `server_tool_use` becomes
a `call` keyed by its own id. Any other block type becomes `unknown` and keeps its bytes. Content given
as a plain string, 4,429 records, becomes one `text` part. An attachment becomes `text` when it has a
`text` field or a string `content`, and otherwise travels whole as a `data` part. This dialect never
marks a part `truncated` or `omitted`.

Where the bytes are, across 62 sessions and 1,100.1 MB of source records:

| What | MB | Share |
| --- | ---: | ---: |
| record ids, usage blocks and the rest of the envelope | 370.0 | 33.6% |
| tool results, the content block | 334.8 | 30.4% |
| reasoning signatures | 146.4 | 13.3% |
| tool results, the `toolUseResult` copy | 83.8 | 7.6% |
| tool inputs | 75.0 | 6.8% |
| injected context | 58.7 | 5.3% |
| message text | 18.4 | 1.7% |
| assistant text | 14.3 | 1.3% |
| reasoning text | 0.1 | 0.0% |

Tool results are 38.1% of the bytes, more than five times the tool inputs. Message text and
assistant text, the text a person reads, are 3.0% together.

**Reasoning signatures are not landed.** A thinking block's `signature` is listed in the record's
`dropped` with its size and the reason, "a provider verifies it; a reader cannot read it". A provider
checks the signature when a block is sent back to it, and no reader can use it. In the table above,
signatures are 13.3% of the bytes and wrap 0.1 MB of reasoning text.

**A tool result's content is not always a string.** Over 20,000 `tool_result` records: string 19,707,
list of image 124, list of text 104, list of `tool_reference` 92. `tool_reference` is an undocumented
block, `{"type":"tool_reference","tool_name":"Monitor"}`, 194 in the corpus. A `result` takes its
`text` from the string, or from the text blocks joined. When there is no text, the part keeps the raw
content as `data`, because the source can be pruned and the landed copy may be the only one. A
reader that treats the content as a string loses the image and reference results.

**A Claude Code result carries `text` and `data` together.** When the record carries a
`toolUseResult` object, the whole object becomes the result part's `data`, beside the text. Both are
kept on purpose. `toolUseResult` has a different shape for each tool, 49 distinct key sets across the
corpus, which flattening would lose, and it has detail the block lacks: stdout and stderr apart, a
patch, a status. It is also the only full copy of long output. Among 516 records whose output spilled
to a file, the content block was shorter than `toolUseResult.stdout` by about 28 KB where it had been
cut. The cost is the `toolUseResult` copy in the table above, 7.6% of the bytes. An `Edit` or
`Write` result gains a second `data` part, a `changes/1` record, see
[Workspace changes](#workspace-changes).

**Four things are left out with no `dropped` entry**, which breaks the
[Dropped](../formats/session-data.md#dropped) rule. They are known gaps:

- a `tool_use` block's `caller` field, `{"type":"direct"}` on all 108,149 tool uses counted;
- a `toolUseResult` that is a plain string, 7.2% of results;
- the raw content of a result with no text, when the record also carries a `toolUseResult` object,
  which takes `data` in its place;
- the other fields of a record that becomes parts, which is a `user` or `assistant` message or an
  attachment.

Such a record lands with `id`, `parent`, `call`, `run`, `continues`, `tool`, `child`, `batch`,
`started_by`, `label`, `time`, `trigger`, `model`, `usage`, `flags`, `from` and its parts, and
nothing else. `requestId`, `isSidechain`, `cwd`, `gitBranch`, `slug`, `version`, `userType` and
`entrypoint` do not land, and neither does the rest of an attachment that has text. They are gone
once Claude Code prunes the source. The in-record `sessionId` and `agentId` do not land either, but
the landed header names the same session and stream, see [Execution streams](#execution-streams).
`cwd` is read only to make a changed file's path relative. A record with no message content and no
attachment travels whole as one `data` part, so all its fields are kept. System records, sidecars,
journal lines and manifests are of this kind.

**An MCP tool use is an ordinary `call`.** Its server is only a prefix on the name,
`mcp__<server>__<tool>`. This was observed on 39 of 26,940 tool uses (0.1%), too few to state as a
rule.

Five record fields come from places the glossary does not list:

- `label`: `aiTitle` on the runtime's title record, else `description` on a child's sidecar, else
  `workflowName` on a workflow manifest.
- `started_by`: a nested child's sidecar `parentAgentId`.
- `tool`: `toolUseResult.toolUseId` on a tool result, else the `<tool-use-id>` of a completion
  notification. The glossary's `tool` row is the step kind of the same name, so it does not list
  this role's source.
- `from`: the record type. `assistant` is `agent`, `user` is `external`, and `system` and
  `attachment` are `runtime`, as is every record of a sidecar, journal, manifest or script. So
  `from: external` also covers every tool result and every `isMeta` record, which sit on `user`
  records. The flags and the parts say which is which.
- `usage`: `message.usage`, the input, output, cache read and cache write tokens.

### Names

Three runtime fields carry names, and the page adds a fourth source.

- The conversation's title is `aiTitle`, on the runtime's title record. The runtime rewrites it as
  the work goes on, so the session node takes the last one written, and
  [asz.view](../formats/asz-view.md#top-level) shows it as `summary.title`.
- A child stream's label is `description` in its `agent-<id>.meta.json`.
- A workflow's name is `workflowName` in its manifest. It is shown on the call that launched the
  workflow.
- A workflow child with no label is named from its run journal's result row: its `surface`, else
  its `summary`, else its `verdict` and the first refuted claim. `asz.view` marks such a stream
  `named_by: journal`. A workflow child's own records never say what it was for. Over 178 children
  of one conversation, 177 opened with byte-identical injected context.

A title was present in 41 of 42 conversations, and `system/turn_duration` on 465 turns, in a sample
whose selection was not recorded. `asz.view` shows a turn's `duration_ms` as the runtime measured it.

## Workspace changes

Which files a tool call changed comes from two places, and lands in one shape, `changes/1`, so a
view shows one Changes tab whichever recorded it.

**The runtime's own patch.** Every successful `Edit` and `Write` result on the main stream carries
`toolUseResult.structuredPatch`, `filePath` and `originalFile`, measured on 2,221 of 2,221 such
results in a 52-session corpus. A `NotebookEdit` result carries `notebook_path`, `original_file`
and `updated_file` and no patch, measured on a run of Claude Code 2.1.260. The adapter copies them
into a `changes/1` record as a second `data` part beside the raw result, with the tool-use id as
its id, `captured_by: claude-code`, `basis: runtime_reported`, the path relative to the record's
`cwd`, the hunks as the runtime wrote
them, or computed from the two versions when it wrote none, and the content before and after
hashed. The raw result stays beside it as the JSON Claude Code wrote, apart from whitespace between
tokens (see [What data holds](../formats/session-data.md#what-data-holds)). A subagent's transcript
carries no such patch: 0 of 453 in the same corpus.

In the 2026-09-12 source sample described on that page, all 32,887 tool result data parts from
main transcripts, 164 from direct subagents and 166 from workflow subagents landed byte for byte.
These counts include the structured result or content that is not text. The earlier encoding
kept 21,569, 43 and 166 of them byte for byte, respectively.

**The plugin's observations.** The [asz Claude Code plugin](../setup/claude-code-plugin.md)
records shell commands, and edits inside subagents, into its own data directory beside Claude
Code's files. The `claude-code-changes` adapter, on by default, finds them the way this adapter
finds transcripts, resolves `plugins/data` under the same Claude Code directory, and lands each
line as a record of kind `changes` under the stream the tool ran in, with the session's own lock
and sequence. Session filters apply to the workspace the records name.

Neither is a step. Assembly leaves a `changes` record out of its stream, so a session folds to
the same nodes with and without them, and the view joins each record to its step by the
tool-use id. The scenario `tests/scenarios/workspace-changes.yaml` checks both paths in both
formats, and that the fold is unchanged.

## What this adapter cannot supply

| | Why |
| --- | --- |
| **Reasoning text** | Claude Code asks the provider not to return it, so most thinking blocks carry only a signature. `unavailable` where the runtime wrote none. See below. |
| **Serialized request** | system prompt, tool schemas and cache annotations are absent from transcripts: 0 files contain `"tools":[`, and `compactMetadata.preservedMessages.allUuids` names 5 ids that exist nowhere on disk. asz produces no input manifest and no `input_of` relation, and reports model-context coverage as `unavailable`, as the model's [adapter contract](../concepts-and-designs/unified-conversation-model.md#adapter-contract) requires. |
| **Injected preamble** | the instruction block prepended to the first user message has no transcript record. Its *data* survives as `attachment` records; its rendered form does not. |
| **Per-call duration and cost** | not written to transcripts. Available via OTLP. |
| **`tool.execution`** | no local record; the call and result are observable, the execution is not. |
| **Retry attempt identity** | derivable only for failures that received an HTTP response: in the first pass, 48 of 556 `api_error` records carried a real request id in `error.requestId`, and transport failures carry none. asz does not derive it yet. |
| **What a shell command changed** | not in the transcript, which holds the command and its output only. Available from the [plugin](../setup/claude-code-plugin.md). The same for an edit inside a subagent, whose transcript carries no patch. |

**Reasoning text** is kept wherever the runtime wrote it, and whether it did depends on the model. In
the first pass, 2,603 of 2,604 files with thinking blocks had empty text. The one exception was a
subagent on `claude-haiku-4-5-20251001`, with 31 blocks of 61 to 1,742 characters. In the same
session at 2.1.227, eleven sibling agents of the same type on `claude-opus-5` kept none. A live
capture showed the cause: the request sends `thinking.display:"omitted"`. Raw-body capture on 2.1.245
redacts the text again, and OTLP events always redact it, so it is not recoverable by any local
mechanism. An empty block is marked `unavailable`, and a `redacted_thinking` block is marked
`redacted`.

The injected preamble was measured on captured request bodies from one development machine. It is
one 8,338-character `<system-reminder>` block wrapping the instruction file, the user's email and the
current date. A search of the transcripts' fields finds it on no record, and subagents receive it
too. The attachment data behind it survives, 88.8% to 98.7% recoverable, but the rendered wrapper and
its ordering do not. The tool list also changes between adjacent calls through deferred loading, from
96 to 116 tools one turn apart, and only a `deferred_tools_delta` attachment records it.

The injected preamble is a **fixed** cost — roughly 8 KB without a project instruction file, up to
~40 KB with one — so its share of a conversation falls as the conversation grows. It is not a
proportional loss. In a real session holding 12,027,973 characters of conversation content it is
about 0.3%. A 26 KB probe session would suggest 99%, and is not representative.

The mapping from injected records to request blocks is **beta-gated and varies between client
versions**. On one machine on one day, two schemes were seen. Without the
`mid-conversation-system-2026-04-07` beta, attachments become separate reminder-wrapped user text
blocks placed before the prompt. That is the reverse of the transcript, which writes the user record
first with its attachments as descendants. With the beta, they merge into one 13,953-character
`role:"system"` message placed after the user message. The active beta list is not recorded, so a
transcript-only reconstruction of the exact request message list is not possible. Verifying
model-context continuity against what was actually sent requires captured provider bodies.

## OTLP (optional second channel)

Claude Code exports OpenTelemetry when `CLAUDE_CODE_ENABLE_TELEMETRY=1` and the relevant exporter
variables are set. It adds **measurement only**:

- per-call `duration_ms` and `cost_usd`, tool durations and error types
- `tool_decision` with a composite `source` (`hook:` / `rule:` / `mode:`)

It contributes **no structure**. Events carry no agent identity — `query_source` is normalised to
`main | subagent | auxiliary`, and the child-completion event carries no agent id at all. Agent
lineage exists only on spans, which require an additional beta flag; several span names present in
the binary are unreachable in shipped builds.

Its content defaults are restrictive, as [Collection](#collection) describes.

### The same metrics from the transcripts

With `metrics: true`, this adapter derives a reconstructed subset of the runtime's own metric
family from what it landed, under the exporter's metric name, so a receiver holds one name
whichever produced the points. Phase one is `claude_code.token.usage`: the usage of a call is its
last fragment's in line order, as the assembler reads it, and only a finished call counts;
`query_source` is `main` for the session's own transcript and `subagent` for a child's; `model`
is `message.model`; `session.id` is the session. The exporter's account, organisation, terminal,
speed, effort and attribution labels are not on a transcript and are not added, and neither are
its auxiliary calls. Points are monotonic delta sums per minute whose windows never overlap, a
double for every type as the exporter writes them, and a call is counted once however many
landed files its records reach. A capture of the exporter's own request is the parity fixture
the derivation is tested against.

What the transcripts do not carry is not derived: cost, per-request latency, tool execution time,
active time, lines of code, commits, pull requests, the session start type, and the tokens of the
runtime's auxiliary calls, which never reach a transcript. Those come from the exporter alone.
The first derivation over a root with history reaches back `metrics_lookback`, 24 hours unless
set. See [Metrics](../setup/export-otlp.md#metrics).

### The runtime's exporter, received

`claude-code-otlp` is the other adapter for this runtime: an OpenTelemetry receiver its
exporter is pointed at, over gRPC or HTTP with protobuf on one port. Phase one lands the metrics
requests it receives in the spool, bytes as received, and `asz push` sends them under asz's
identity; logs and traces are accepted and dropped. It lands no transcript and reads none, so it
adds no structure; the two adapters meet only in the spool, and `metrics` is on for one of them.
The runtime's events, with the latency and cost a transcript never carries, are the second phase,
and the question there is whether they land as Session Data joined to `llm.call` by the request
id or travel untouched.

The receiver speaks OTLP because Claude Code has exactly one outward protocol. Its exporter setting
accepts `console`, `otlp` or `prometheus` for metrics, and `console` or `otlp` for logs and traces.
Any other value throws. There is no webhook and no custom sink. This was read from the Claude Code
binary. The version was not recorded.

## Local data hazards

Behaviours that will break a naive reader:

- **Session ids are not reliably lowercase.** 2 of 47 transcripts used an uppercase UUID as their
  session id, for example `41090DAB-113C-…`. Discovery accepts either case, and joins must be
  byte-exact and never case-folded.
- **Lines can be very large.** The longest measured is 4,629,074 bytes, and 22 lines are over
  1 MiB, on the 60-session storage root that [Storage Root](../formats/storage-root.md#append-cursors)
  measured on 2026-09-11. Files reach 59 MB. A scanner with a 64 KB token limit fails silently.
- **`cwd` and `gitBranch` are per-record**, not session constants; one session showed 52 distinct
  `cwd` values. Only `sessionId`, `entrypoint` and `userType` are reliable per-file constants.
- **A large share of lines carry no record id at all.** In the first pass, 38,340 of 133,887
  main-transcript lines (28.6%) had no `uuid`, across 14 metadata-only record types, none of them
  context-bearing. A parser assuming every line has one will fail. asz lands such a record at its
  position, and never treats two records without an id as copies of each other.
- **Some files contain no conversation records at all.** In the first pass, 177 of 2,935 files held
  no `user`, `assistant` or `system` record, only metadata types. asz treats every
  `<session-id>.jsonl` as a session regardless: it lands and assembles the file, and the list shows
  the result as a conversation with no Talks. How many of the 177 were main transcripts was not
  measured.
- **Never match a tool-use id on its prefix.** An id starting `srvtoolu_` appears in the corpus, but
  the 2,970-file pass found it only inside a `WebSearch` result payload, one level below the tool
  call, where nothing joins on it. The join key is the exact id.
- **The corpus is live.** Files appear and grow during a scan; counts are snapshots.
- **`toolUseResult` is a main-stream enrichment.** On workflow children only the error-string form
  survives, and the key is *absent* rather than null.
