# Design Plan 01 — Claude Code Collection and Raw Storage

**Status:** draft for discussion. Not implemented.
**Scope:** how raw source data is acquired and stored, per session. The per-session processing
pipeline is Plan 02; conversation output (bundle/export/preview) is Plan 03. Neither is settled
here, and nothing in this document should be read as fixing them.

**Collection phasing (decided):**

| | Channel | Posture | Delivers |
| --- | --- | --- | --- |
| **Phase 1** | local logs — `~/.claude/projects/**` | pull (file tailing) | the whole conversation: structure, content, lineage, epochs. No configuration, and it works on history that already exists. |
| **Phase 2** | OTLP | push (receiver) | measurement only: per-call `duration_ms` and `cost_usd`, tool durations, permission decisions. Requires user configuration and captures only from the moment it is enabled. |

Phase 1 is self-sufficient: it is the product. Phase 2 is additive and changes nothing in the Phase 1
layout — `log-` and `trace-` are already reserved prefixes (§2.2). Raw provider bodies
(`OTEL_LOG_RAW_API_BODIES`) are a separate opt-in, orthogonal to both phases, and are the only route
to a verified `input_manifest` (§1.8, §1.13).

**Relationship to the earlier design draft (v0.8):** that draft was written when the project was
still called Agent Sessionizer, and it is not part of this repository. v0.8 is the direction, not a
specification.
Where this plan contradicts it, the observed evidence wins and the contradiction is deliberate.

**Evidence basis:** a live corpus of ~2,920 JSONL files (~1.2 GB) across 38–43 project directories —
52–61 main transcripts, ~107 direct subagent transcripts, ~2,580 workflow-agent transcripts, 176
workflow journals — plus the 2.1.245 binary, plus live runs. Every measured claim was produced by
one agent and then re-measured by an independent adversarial verifier on a larger sample; where they
disagreed, the verifier's number is used and the disagreement is noted.

**Version coverage caveat (applies to everything below):** the corpus spans **only Claude Code
2.1.220 – 2.1.251** (14 versions). No 2.0.x transcript exists on this machine. Every claim is
therefore *untested* below 2.1.220 rather than verified stable.

**Corpus liveness caveat:** the corpus grew during measurement (main transcripts went from 60 to 61
inside one 20-minute run). Absolute counts are snapshots. A collector must tolerate files appearing
and growing mid-scan.

---

## 1. Evidence base — what Claude Code actually exposes

### 1.1 File layout

```
~/.claude/projects/<slugified-cwd>/<session-id>.jsonl                    main stream
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.meta.json
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/journal.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/wf_<run-id>.json     run manifests
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/scripts/
```

Note workflow agents live under `subagents/workflows/`, **not** under `<session-id>/workflows/` —
that sibling directory holds run manifests and scripts only.

`agent-*.jsonl` ↔ `agent-*.meta.json` is a perfect 1:1 across all ~2,686 pairs, zero orphans in
either direction.

### 1.2 Record model — four corrections to the obvious reading

**A line is not always one content block.** 156 records corpus-wide carry a multi-element
`message.content` array on one line — 11 assistant and **145 user**, including 37 of shape
`[image, text]` where `content[0]` is a base64 PNG. Reading `.message.content[0]` renders a base64
image blob as the user's prompt, and silently drops `tool_use` blocks on the assistant side —
breaking the tool join. **Always iterate the full array.** Not version-specific; observed in
2.1.220, .221, .223, .227, .237.

**Fragments of one response are not always consecutive.** A `user`/`tool_result` for an earlier
parallel `tool_use` is written in between, and the `parentUuid` chain runs *through* it. Assembling
by walking consecutive lines, or assuming `parentUuid` adjacency implies same-message, mis-assembles
roughly 1,000 manifests.

**`uuid` is not unique within a file.** 1,530 duplicated `(file, uuid)` pairs, confined to 10 main
transcripts, **multiplicity exactly 2 — never 3**. Copies differ in `slug`, `promptId`, and
sometimes `cwd`/`gitBranch`/`parentUuid`. The ceiling of 2 is the useful engineering fact: drop duplicates by
`(file, uuid)` terminates.

> These blocks were initially read as resume replays. They are not — see **§1.12b**, where they turn
> out to be the corpus's strongest chain-external evidence. Duplicate removal still applies; the *interpretation*
> changes.

> **Duplicate rule:** keep the **first** occurrence for position, merging in fields the later copy adds
> (e.g. `slug`). Keeping the last relocates up to 415 records from their true chronological position
> to the resume point and scrambles line-order reconstruction. Timestamps are identical between
> copies, so they cannot arbitrate.
>
> *Contested:* one pass measured "later copy never richer", the verifier measured "later copy larger
> in a third to half of pairs." Unresolved — see §1.9. The keep-first-and-merge rule is safe under
> either reading.

**`cwd` and `gitBranch` are per-record write-time stamps, not session constants.** One session
showed 52 distinct `cwd` and 14 distinct `gitBranch` values. Only `sessionId`, `entrypoint` and
`userType` are reliable per-file constants (`slug` varies across replay copies).

### 1.3 Provider calls

**Group by `(file, message.id)` — not by `requestId`.** `message.id` is present on 115,162/115,162
assistant records; `requestId` is absent on 92 including genuine provider calls, and 2 `requestId`s
map to two `message.id`s because a `<synthetic>` refusal companion reuses the real call's
`requestId`. Grouping by `requestId` puts a locally fabricated block inside a real provider call.
Neither key ever appears in more than one file.

**Synthetic filter:** `.message.model == "<synthetic>"`, equivalently `.message.id` not matching
`^msg_`. Both agree 97/97. Do **not** use `requestId == null` — 24 of 97 synthetics carry a non-null
`requestId`, and 2 genuine calls have no `requestId` key at all.

**Ordering is by line, never by timestamp.** Timestamps are monotone within a group, but 372 groups
have tied timestamps, and at file level 2.76% of timestamped records are out of order even after
duplicate records.

**Usage: take the last fragment. Never sum, and do not assume fragments agree.**

| | behaviour |
| --- | --- |
| main transcripts | identical final usage stamped on every fragment (14,167/14,171 groups) |
| subagent transcripts | **streaming partials** — `output_tokens: 1, 2, 3 …` on all but the last |

`output_tokens` is monotone non-decreasing in line order with last == max in **56,524/56,524**
groups, zero counterexamples. Summing inflates by a mean of 2.14× (max 10×). This is the single most
likely source of silently wrong numbers.

**Terminal-fragment test:** the terminal fragment is identified by the presence of
`{iterations, server_tool_use, speed}`. Prefer `speed` — `iterations` is JSON `null` on the 97
synthetic records, so a `has("iterations")` test returns true for them.

**7.14% of calls have no terminal record at all** (main 0.02%, subagent 13.70%) — usage and
`stop_reason` are permanently unavailable for those. The rate tracks subagent workload composition,
not version.

**Fragment count distribution:** 1:18,318 · 2:19,918 · 3:16,666 · 4:1,386 · 5–12:236. Mean 2.04,
max 12.

**Useful streaming invariant:** after duplicate uuids are removed, no two provider calls' line ranges overlap
(0 of 56,524). A group's fragments occupy a contiguous line span interrupted only by its own
`tool_result` records. A streaming collector can close a ProviderCall the moment a line with a
different `message.id` arrives.

**Retry attempts are partially derivable.** `type:"system", subtype:"api_error"` records carry
`retryAttempt`/`retryInMs`/`maxRetries`. 48 of 556 also carry `.error.requestId` — a real `req_…`
id, present exactly on failures that got an HTTP response (e.g. 529), absent on transport failures.
Those 48 are disjoint from all successful requestIds, so each failed attempt has its own id.
`.error.status` is present on 50 of 556.

**Content is complete in transcripts — with one exception.** User prompt text, assistant prose, tool
inputs and tool results are all stored verbatim and untruncated. This is the reason transcripts, not
OTLP events, are the v1 channel: events redact `user_prompt` / `assistant_response` by default,
truncate content at ~60 KB, and omit tool inputs unless `OTEL_LOG_TOOL_DETAILS` is set.

**The exception: thinking text is stripped.** The signature is retained, the reasoning is not:

```json
{"type":"thinking","thinking":"","signature":"CAISkwMKpgEIERgCKkBzgtqcryyQ3cbpBYwiBCwKlBijnqr7oxML…"}
```

Measured corpus-wide: **2,603 of 2,604 files containing thinking blocks have empty thinking text**
(54 main + 2,549 subagent), across every version 2.1.220–2.1.251. Exactly one subagent file retains
real text (31 blocks, 61–1,742 chars) — unexplained, and worth chasing.

**Cause identified, and it is not a persistence choice.** A controlled comparison inside one session
(`221ac729…`, all agents at 2.1.227, same agent type) isolates the model as the only variable:
`claude-haiku-4-5-20251001` retained 31 thinking blocks with real text (61–1,742 chars); eleven
sibling `claude-opus-5` agents retained none. A live capture then showed the mechanism — the request
body carries

```json
"thinking": {"type":"adaptive","display":"omitted"}
```

Claude Code *asks the API not to return thinking text*. The signature comes back, the reasoning does
not.

**Verdict: thinking text is unrecoverable by any local mechanism.** Two independent redactions stack:
the request suppresses it at the API, and the raw-body export redacts it again — a live capture on
2.1.245 shows the response's thinking block as literally `"<REDACTED>"` (10 chars) beside a 596-char
signature. OTLP events redact it unconditionally too. Treat reasoning content as `unavailable` in
the coverage model and do not design around recovering it.

Consequence: a Talk tree can show *that* the model reasoned, with duration and token cost, but not
*what* it reasoned.

**Injected context is captured.** The `attachment` record type — the third-largest in the corpus at
~21,000 records, 32 distinct `attachment.type` values — carries what the harness pushed into the
conversation alongside the human turns:

```
skill_listing   deferred_tools_delta   agent_listing_delta   nested_memory
mcp_instructions_delta   command_permissions   directory   date_change
task_reminder   read_truncation_notice   selected_lines_in_ide   plan_mode_exit
```

So a reconstruction is not limited to human/assistant turns: skill catalogues, tool-availability
deltas, memory injections, MCP instructions, IDE state and permission changes are all on disk in
chain order.

**But the transcript is provably not a superset of the model's context.** Three things are absent
from every transcript — the **system prompt**, **tool schemas** (only names are recorded; 0 files
contain `"tools":[`), and **thinking text** (§1.3, which matters extra because thinking blocks are
re-sent in later requests and were therefore genuinely in context). And there is direct proof rather
than inference: **`compactMetadata.preservedMessages.allUuids` contains 5 uuids that exist nowhere
on disk** (§1.6) — records the runtime held in context and never wrote.

**Consequence for `input_manifest.state`: `truncated`, never `full`, and never `unavailable`.**
`message_refs[]` can be built by walking the chain, so a manifest exists; `system_block_refs[]` and
`tool_definition_refs[]` cannot. Critically, the message membership so derived is **inferred, not
observed** — nothing in the transcript proves those messages were the ones serialized into the
request. Verifying the §2.2 continuity invariant requires raw bodies (§1.8); no amount of file
reading substitutes.

### 1.4 Tool joins — effectively perfect, once duplicates are removed

Across 2,924 files / 356,583 records / 207,984 tool blocks:

- Every `(file, tool_use_id)` maps to exactly **one** `tool_result` record once duplicates are removed and exactly
  **one** assistant `tool_use` record. 103,503 keys, all cardinality 1. Zero fan-out, zero orphans,
  zero cross-file leakage.
- `sourceToolAssistantUUID` is present on **103,503/103,503** tool_result records, always equals
  `parentUuid`, and always resolves to the assistant fragment carrying the matching `tool_use`.
  Zero mismatches. It is fully redundant with `parentUuid`, so it adds no join power — but it is a
  free consistency check.
- **Before duplicates are removed** the join is *exact_ambiguous* for 441 ids (882 of 64,722 tool_results, 1.4%),
  purely from replay copies. Removing duplicates is a precondition, not an optimisation.
- `tool_use_id` is not only `toolu_` — 35 values are `srvtoolu_` (server-side tools such as web
  search). A `^toolu_` regex silently drops them. 0 cross-file collisions on 103,940 distinct ids.

**Parallel tool batches have two on-disk layouts.** Layout A (80.5%) is strictly linear and
interleaved — `tool_use, tool_result, tool_use, tool_result` — producing no fork. Layout B forks the
`parentUuid` chain. Only the per-pair invariant generalises; do not assume a fork to group a batch.

**`toolUseResult` is a main-transcript enrichment.** Present on 100.0% of unique main-transcript
tool_result records; on workflow subagents it is **exclusively** the error-string form (zero
object-typed). Structured tool telemetry (`structuredPatch`, `stdout`/`stderr` split, agent usage)
exists only for the parent stream. The key is *absent*, not null, on child records — a
`toolUseResult === null` test never fires.

**`tool_result.content` is not always a string.** Over 20,000 records: string 19,707, list-of-image
124, list-of-text 104, list-of-**`tool_reference`** 92 — an undocumented block type of shape
`{"type":"tool_reference","tool_name":"Monitor"}`, 194 corpus-wide.

**Long output spills to a file, but the field is unreliable.** Only 45 of 516 spill records carry
`.toolUseResult.persistedOutputPath`; the path is recoverable in 506 of 516 by parsing
`Full output saved to: <path>` out of the preview text. Also, the content block is truncated
relative to `toolUseResult.stdout` by ~28 KB in the truncation cases — `toolUseResult.stdout` is the
only authoritative copy of long output.

**Interrupts do not break the tool join** — the `[Request interrupted by user]` record is written
*after* the tool_result. But the next user turn can re-parent to an earlier uuid, which is a real
`ExecutionStream` discontinuity.

Zero historically unresolved tool_uses exist in this corpus — but that is a property of *this*
corpus (no session was killed mid-call), not an invariant. An `unresolved` state is still required.

### 1.5 Subagents — three spawn mechanisms, not one

This is where my earlier write-up was most wrong. There is no single field spanning all three.

| Spawn kind | Count | Parent→child join |
| --- | --- | --- |
| **Agent tool** | 89 | `meta.json.toolUseId` → `tool_use` name `Agent` (89/89), *and* parent's `toolUseResult.agentId` |
| **Skill fork** | 16 (4 sessions) | `toolUseResult.agentId` with `status:"forked"` only — meta.json has **no** `toolUseId` |
| **Workflow** | ~2,579 | `Workflow` tool_result's `toolUseResult.transcriptDir` (absolute path of the run dir) + `toolUseResult.runId`; and `journal.jsonl` `{type:"started", agentId}` → 2,583/2,583 bijection |

An earlier pass claimed workflow children have no parent `tool_use` and that `journal.jsonl` is
their sole causal record. **Both refuted** — `toolUseResult.transcriptDir` is an exact_unique join
from a parent tool_use straight to the directory holding every workflow child.

Confirmed by full re-scan of all 2,686 agent files (218,393 records):

- filename `agentId` == in-record `agentId`, 2,686/2,686; zero collisions; zero files with >1 `agentId`
- zero files with >1 `sessionId`; **zero files whose `sessionId` differs from the parent directory** —
  the child always shares the parent's session, with no deviation
- `isSidechain`/`agentId` partition the two file classes perfectly: 0 sidechain records in mains,
  0 top-level `agentId` in mains
- `agentId` conforms to `^a[0-9a-f]{16}$` in 224,130/224,130 occurrences
- `spawnDepth`: 2,654×1, 31×2, 1 null. **No depth ≥ 3 exists.** All 31 depth-2 agents carry
  `parentAgentId`, resolving to Skill-forked parents.
- the child's first user record is a bare string **byte-identical** to the parent `tool_use`
  `input.prompt`, 89/89
- `"Task"` does not exist as a tool name anywhere — the tool is `Agent`

**Async return.** `toolUseResult.status:"async_launched"` is the launch acknowledgement, distinct
from the result — the field distinguishes them explicitly. Completion arrives as a
`<task-notification>` XML blob whose `<task-id>` **is** the agentId.

> **Caveat that breaks my earlier "exact end-to-end" claim:** the notification's `<tool-use-id>` is
> *not* reliably resolvable in the transcript it lands in — **28 of 54 fail**, because nested
> children notify the MAIN session while their spawning `tool_use` lives in an agent file. For
> depth-2 the split is clean and exact: all 31 toolUseIds resolve in the parent *agent* file, all 31
> agentIds appear as `<task-id>` in the *main* transcript. So the edge is exact but **cross-file** —
> resolution must search the session's whole file set, not one file.

`<status>` vocabulary is `completed | failed | killed | stopped`, and **1,183 notifications carry no
`<status>` element at all** — parsing it unconditionally will null-deref.

`agentType` does **not** identify a workflow spawn (a workflow step can request a named agent type).
The reliable discriminator is on-disk path / journal membership.

**The journal leads the filesystem.** For in-flight runs, journalled `started` agentIds had no
`agent-*.jsonl` yet — one even had a `result` journalled while its transcript was still missing.
A collector meeting a live session must treat this as normal, not as corruption.

### 1.6 Context epochs and session identity

**Compaction is explicit and exactly linked.** `{"type":"system","subtype":"compact_boundary"}` with
`compactMetadata`. 26 boundaries corpus-wide, zero in any subagent file.

The critical field is **`logicalParentUuid`** — an explicit pointer to the last pre-compaction
message, present on 26/26 boundaries and on **no other record type anywhere** (0 occurrences among
133,347 other main records and 221k+ subagent records). Exact rule, 26/26:
`logicalParentUuid == compactMetadata.preservedMessages.allUuids | last`.

So epochs *are* explicitly chained; `parentUuid: null` on the boundary is a DAG-root display
convention, not an absence of linkage. This is precisely the explicit epoch boundary v0.8 §4.2
demands, and it means compaction never has to be inferred.

Also established:

- `boundary.uuid == summary.parentUuid`, `isCompactSummary == true`, summary is the **very next
  line** — 26/26, zero exceptions.
- The summary's timestamp is **always earlier** than its own boundary's, by 1 ms to 1,097 ms, 26/26.
  **Never order an epoch boundary by timestamp.**
- `preservedMessages.allUuids` is a strict superset of `uuids`. `uuids` resolves 26/26; **`allUuids`
  contains 5 uuids that exist nowhere on disk** — records held in context but never written. Since
  `logicalParentUuid == allUuids | last`, one boundary's `logicalParentUuid` is itself unresolvable.
  Both must be typed unresolved-capable.
- The preserved segment does **not** abut the boundary: the last preserved uuid is immediately
  before it in only 16/26; gaps reach 705 records. Resolve by uuid only, never by file proximity.
- `trigger` is `auto` 24 / `manual` 2. `preCompactDiscoveredTools` is absent on 2/26 and `isMeta`
  absent on 5/26 — both must be optional in a Go struct.

**Resume appends to the same file with the same sessionId.** Proven by a 659-hour gap where
`version` jumps 2.1.220 → 2.1.251 and the chain still links. Across all 60 main + 2,851 subagent
files: **zero dangling parentUuids, zero pointing cross-file.** Root count per file =
`1 + N_compactions + N_replay_restarts`.

**`/clear` starts a new sessionId and a new file**, and is detectable at the **head of the successor
file** (line 3–4) as a fixed 3-record chain: a root `user` record with `isMeta:true` and
`<local-command-caveat>`, then a `user` record whose content is the string
`<command-name>/clear</command-name>`, then `system/local_command` with empty stdout. So "this
session was born by clearing a predecessor" is detectable — though the record carries no pointer to
the predecessor's sessionId.

**There is essentially no durable on-disk conversation identity spanning sessionIds.** Candidates,
all weak:

| Candidate | Verdict |
| --- | --- |
| `bridgeSessionId` | spans 2 sessionIds in 1 of 20; can be the empty string; one session has two values. Not 1:1. |
| `.session_id` (snake_case, ≠ `.sessionId`) | **contested** — see §1.9 |
| `slug`, `ai-title`, `cwd`, `gitBranch` | per-record or per-session, mutate mid-session |

This confirms v0.8's core premise: **the conversation ID must be supplied by the application.**
No local heuristic is sound.

### 1.7 OTLP — traces exist but are largely dead code

Claude Code emits all three signals, on very different terms. Three instrumentation scopes:
`com.anthropic.claude_code` (metrics), `com.anthropic.claude_code.events` (logs),
`com.anthropic.claude_code.tracing` v1.0.0 (spans).

Metrics (8 counters) and logs (~25 events) need only `CLAUDE_CODE_ENABLE_TELEMETRY=1` plus the
relevant exporter var.

**Traces are double-gated:** `CLAUDE_CODE_ENABLE_TELEMETRY=1` **and**
`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`, plus `OTEL_TRACES_EXPORTER`. Without the beta flag every
`startSpan` returns a no-op dummy span. A first-party managed-config toggle can set the beta flag
for spawned sessions, so it is reachable from configuration, not only from a raw env var.

> **Correction to my earlier claim.** I wrote that `claude_code.subagent.spawn` and
> `claude_code.compaction` matter to the design. **They are dead code for external users
> in 2.1.245** — gated behind a hardcoded-false function and an Anthropic-internal endpoint path,
> confirmed in 2.1.224 too. Only **five** span names are reachable: `claude_code.interaction`,
> `.llm_request`, `.tool`, `.tool.blocked_on_user`, `.tool.execution`.
> `.subagent.spawn`, `.compaction`, `.hook`, `.bash.subprocess`, `.mcp.rpc` are unreachable.

The decisive consequence for lineage: **events carry no `agent_id`.** Agent lineage
(`agent_id`, `parent_agent_id`) exists **only on spans** — i.e. only under the enhanced-telemetry
beta. On the metrics/logs path there is no subagent identity, no Conversation ID, and no Segment
concept at all.

What events *do* carry joins cleanly: `session.id`, `prompt.id`, `message.uuid`, `request_id`,
`tool_use_id` — all with exact transcript counterparts. Two caveats: the slash-command
`user_prompt` emit site emits **no** `message.uuid` (so that join is unresolved for every
`/command`), and `tool_decision.source` is a composite string (`hook:…`, `rule:…`, `mode:…`) that
must be split on `:`, not treated as an enum.

Also noted: an always-on 300 s metric reader exists **outside** the telemetry guard for
enterprise/team accounts.

### 1.8 Raw provider bodies — no proxy needed

**`OTEL_LOG_RAW_API_BODIES=file:<dir>` is a first-class, undocumented raw-body dump.** Verified live
on 2.1.245 with subscription OAuth: 12 provider calls captured across 4 sessions including a
subagent spawn. It writes the fully-serialized Messages request params to
`<dir>/<randomUUID>.request.json` and the assembled response to `<dir>/<req_…>.response.json`.

The joins are exact:

- **The response file's basename *is* the Anthropic request-id, and equals the transcript's
  `.requestId`.** Independently confirmed by MITM: the proxy saw response header
  `request-id: req_011CeZxce27ffvNEPbBWiUUx` and the transcript wrote the same value.
- `requestId → sessionId` is 1:1 with zero collisions across 85,652 distinct ids.
- The request body carries `.metadata.user_id` holding the session_id.
- **`.diagnostics.previous_message_id` chains request N to the response of request N−1 by `msg_`
  id** — the ordered-model-input chain the design needs, materialized in the wire body itself.

**Independently reproduced on 2.1.245 with subscription OAuth.** A live capture in this project
confirms the mechanism and shows the request body is far richer than the design assumed. Top-level
keys: `betas, context_management, diagnostics, max_tokens, messages, metadata, model, output_config,
stream, system, thinking, tools`.

| Field | Observed |
| --- | --- |
| `system` | array of 4 blocks, 11,765 chars total — the complete system prompt |
| `tools` | 13 tools **with full `input_schema`** |
| `messages` | the exact ordered array as serialized |
| `cache_control` | present, with ttl and scope, e.g. `{"type":"ephemeral","ttl":"1h","scope":"global"}` |
| `metadata.user_id` | a JSON string holding `device_id`, `account_uuid`, **`session_id`** |
| `diagnostics.previous_message_id` | the chain to the prior response |

**Three independent exact joins back to the transcript**, all verified on the captured pair:

1. the response filename is the request-id — found in the transcript (2 fragment lines)
2. `metadata.user_id.session_id` — resolves to a real transcript file
3. `system[0]` is an `x-anthropic-billing-header` block carrying `cc_prompt_id=…`, which equals the
   transcript's `promptId`, plus `cc_version` and `cc_entrypoint`

This removes the largest and most privacy-fraught piece of the design, and makes
`input_manifest.state = full` genuinely reachable — system blocks, tool definitions, ordered message
refs and cache breakpoints all present as *observed* rather than inferred, which transcripts alone
can never do (§1.3).

**One asymmetry to plan around:** the response file is named `<request-id>.response.json`, but the
request file is named by a client-side random UUID (`<uuid>.request.json`) — the request-id does not
exist until the response arrives. Pairing a request to its response therefore needs either the
`api_request` event (which carries both `client_request_id` and `request_id`) or the body's own
`diagnostics.previous_message_id` chain. `clientRequestId` is absent from transcripts entirely.

**Thinking is redacted here too** — the captured response's thinking block is `"<REDACTED>"`
(§1.3). Raw bodies do not recover reasoning.

**The proxy route is a measurement-perturbing observer and should not be the default.** With
`ANTHROPIC_BASE_URL` pointed at a recorder, Claude Code drops the `cache-diagnosis-2026-04-07` and
`advanced-tool-use-2025-11-20` betas, loses `.diagnostics` entirely, and collapses two
globally-scoped system `cache_control` breakpoints into one. It also hard-fails with 403 under OAuth
unless the forwarder speaks HTTP/2. It remains useful as a cross-check, not as the collection path.

### 1.9 Contested and unresolved

Recorded rather than silently resolved:

1. **`.session_id` (snake_case) as a cross-session identity.** One pass calls it a
   conversation-lineage root surviving `/clear` and spanning 4 sessionIds over 13 days, gated by
   entrypoint (present on 11,721 of 12,959 `cli` records, 0 on `claude-vscode`/`sdk-cli`). Another
   measures 15 distinct `(sessionId, session_id)` pairs of which **11 agree**, with all 8,037
   disagreements traced to **one stale sticky value** inherited by 3 sessions — and shows zero uuid
   overlap with the claimed ancestor. Needs a decisive test before any use.
2. **Whether the later copy of a duplicated uuid is ever richer.** Passes disagree. The
   keep-first-and-merge rule is safe either way, but the disagreement should be settled.
3. ~~Does the raw-body dump retain thinking text?~~ **SETTLED — no.** Live capture on 2.1.245: the
   response's thinking block is `"<REDACTED>"`. Combined with the request's
   `thinking.display="omitted"`, reasoning text is unrecoverable by any local mechanism (§1.3).
   *Remaining sub-question:* raw-body behaviour under concurrency and inside subagents is still
   unmeasured.
4. ~~Why one subagent file retains thinking text?~~ **SETTLED — the model.** Controlled comparison
   within one session at one version isolates it: haiku-4-5 retains text, opus-5 does not (§1.3).
5. **Whether transcripts are ever truncated or rotated.** All evidence says append-only, which the
   storage design depends on (§2.6). Not proven.
6. **Hook `agent_id` coverage** — reported present on every hook fired inside a subagent and absent
   on the main thread, from 5 live headless sessions. Needs wider confirmation.

### 1.10 Hooks — the best lineage surface, with a sharp edge

31 hook events, extracted from the binary's registry and verified by 5 live headless sessions with
all events wired. The common envelope carries `session_id`, `transcript_path`, `cwd`, `prompt_id`,
`permission_mode`, **`agent_id`**, `agent_type`, `effort`.

`agent_id` is present on every hook fired **inside** a subagent and absent on the main thread —
which is exactly the E0/E1 stream discrimination transcripts blur. The parent→child edge is
delivered twice: `SubagentStop.agent_transcript_path` names the child file directly, and
`PostToolUse(tool_name="Agent").tool_response.agentId` pairs the parent's `tool_use_id` to the
child's `agent_id` (verified unambiguous with 3 concurrent subagents).

Compaction is fully instrumented: `PreCompact{trigger, custom_instructions}` → `SubagentStop`
(a phantom internal agent with `agent_type:""`) → `SessionStart{source:"compact"}` →
`PostCompact{trigger, compact_summary}`.

Three hard limits:

- Hooks are **synchronous and on the critical path** (~5.6 ms/invocation measured, 60 s default
  timeout). A collector hook must never do work inline.
- The naive `cat >> spool.jsonl` pattern **demonstrably corrupts lines above ~64 KB** under
  concurrent subagents. Appends must be single-`write(2)`-sized or go through a serializing writer.
- Hooks never expose `message.id` or `requestId`, so they cannot produce ProviderCall manifests.

Note the published docs are ahead of and inconsistent with the shipped binary — code to observed
payloads. Also: the current `settings.json` on this machine has **no hooks block**, so no
hook-based collection point exists yet.

### 1.11 Sidecar surfaces worth collecting

| Surface | Verdict |
| --- | --- |
| `file-history/<sessionId>/<sha256(abs_path)[:16]>@vN` | **Collect.** Directory name *is* the sessionId (26/26); blob name verified by recomputation. `file-history-delta.messageId` is the exact uuid of the assistant `tool_use` line that performed the edit (59/59). |
| `history.jsonl` | **Collect, with care.** Neither complete nor a superset: 37 of 52 transcripts have zero history lines, but 330 of 345 sessionIds exist *only* here (transcripts pruned). |
| `~/.claude.json` `.projects[<cwd>]` | **Collect.** Carries `lastSessionFirstPrompt` (verbatim first human prompt) and `lastSessionId`. Entry shapes vary 9–35 keys. |
| `telemetry/1p_failed_events.*.json` | **Optional.** Not OTLP — a proprietary `ClaudeCodeInternalEvent` NDJSON spool of *undelivered* events, 41 `tengu_*` names, numeric payload base64-encoded in `additional_metadata`. Useful for cache-breakpoint and context-size signals absent elsewhere. |
| `sessions/<pid>.json` | **Optional.** Live process registry, deleted on exit. `bridgeSessionId` joins the transcript's `bridge-session` record only after stripping a `session_` vs `cse_` prefix difference. |
| `shell-snapshots/`, `session-env/`, `backups/`, `paste-cache/` | **Skip.** `session-env` is empty; `backups` rotates in minutes; 91% of paste-cache hashes have no surviving blob. |

Two traps worth naming:

- **`file-history` deltas and snapshots must be unioned.** Deltas only ever name `@v1` blobs (337);
  every blob at `@v2…@v49` (1,667 of 2,004) is reachable **only** through
  `file-history-snapshot.trackedFileBackups`. Neither surface alone is complete. The version counter
  is per-`(session, file)`, not per-file, so the exact key is `(sessionId, backupFileName)`.
- **`history.jsonl` `.pastedContents` has two shapes.** 148 of 305 carry a full inline `content`
  string (≤1,006 chars); the rest carry only `contentHash`. Reading only `contentHash` drops half
  the payloads.

### 1.12a Reconstruction: the `parentUuid` graph is a DAG, not a chain

**This is the finding with the largest implementation consequence.** Parallel tool calls fork the
graph: `tool_use#1 → { tool_use#2 (same message.id), tool_result#1 }`. An ancestor walk from the next
provider call traverses one branch and silently drops the other `tool_use` and its `tool_result`.

> **33,798 of 86,687 provider calls (39%) reconstruct under ancestor-walk to a message list carrying
> a `tool_use` with no matching `tool_result` — a body the Messages API would reject.**
> Line-order reconstruction is balanced in 2,622 of 2,623 tool-bearing files (the one exception being
> a live in-flight file at snapshot time).

**Rule: assemble by `(file, message.id)` grouping plus line order within an epoch. Never by ancestor
walk.** Treat a fork as a topological hint, not a branch to choose between.

Supporting measurements:

- 4,073 fork nodes corpus-wide. **Every fork has exactly 2 children, never more.** Shapes:
  `assistant:tool_use + user:tool_result` = 3,905; `assistant:* + system:api_error` = 160; 3 others.
- The *sequential* multi-tool case is a plain chain (`tool_use1 → tool_result1 → tool_use2 → …`,
  all one `message.id`). Only parallel dispatch forks.
- Fork rate is 0.49% in main transcripts but **1.69% in subagents**, which hold ~69% of all provider
  calls — so main-transcript-only measurement understates exposure. 31.8% of subagent calls carry
  ≥2 `tool_use` blocks versus ~3% in main.
- Fragments of one `message.id` are **never** interleaved with fragments of a different one
  (0 of 86,716 calls), which is what makes `message.id` grouping a genuinely independent second
  derivation from the chain.
- Order inversion: for prompt-bearing turns the request puts reminders **first** and the user's text
  **last**, while the transcript writes the user record first with attachments as descendants. For
  tool_result turns the two orders agree. No chain walk discovers this.

### 1.12b The duplicate-uuid blocks are evidence, not noise

§1.2 records these as resume replays. **That framing is wrong and understates them badly.**

Measured across all 10 affected files: the re-emitted block is **exactly** the set of user/assistant
records lying before it that are *off* the following compaction boundary's ancestry.
`dup_not_off = 0` in 10/10 files; `off_not_dup = 0` in 8/10.

Claude Code is dumping precisely the context members its own `parentUuid` chain fails to reach. That
makes these blocks an independent, chain-external recording of the real message set at 10 sites, and
it establishes the reconstruction rule directly:

> **context = chain ancestry ∪ off-chain siblings**

The 2 exceptions are equally informative: 1f04957c has 330 and e84d2dab 37 off-chain records that
were *not* re-emitted, clustering in whole abandoned runs. So off-chain records partition cleanly
into **(a) parallel-tool siblings — re-emitted, genuinely in context** and **(b) abandoned or errored
branches — not re-emitted, genuinely dropped.** That is ground truth for a decision the assembler
would otherwise have to guess at.

Also corrected: all 10 duplicate blocks terminate on the line immediately before a `compact_boundary`;
only 2 of 10 are dense contiguous prefix replays; 0 of 1,530 lie in the ancestry of the following
boundary's `logicalParentUuid`.

### 1.12c What the continuity invariant can and cannot see

Established by mutation testing — corrupting the reconstruction and measuring whether the check
notices.

**Blind to message content, completely.** Ten destructive mutations moved the failure count by
**exactly 0** on both populations: `content[0]`-only, dropping thinking blocks, grouping by
`requestId`, dropping the `isCompactSummary` record, dropping `isMeta` records, blanking every
tool_result payload, blanking every text block, replacing blocks by their type string, replacing
blocks by one indistinguishable token, and collapsing every record to a single indistinguishable
block.

Evaluated against transcripts, the check is therefore a function of exactly one thing: **the
role-segmented record-count sequence along the chain.** It cannot observe a single byte of any
message body.

**Loud about intra-message order.** Ordering a message's fragments by uuid instead of line number
takes main failures 178 → 9,714 and subagent 518 → 30,437.

> v0.8 must describe this as a **structural cross-check on
> `(message.id grouping × parentUuid tree × fragment order)`** — never as checking the request body.

**Do not use it as a ranking oracle.** It scores three known-wrong reconstructions *better* than the
correct one: keep-LAST duplicate handling (−19), dropping `<synthetic>` assistants (−13), and line-order
reconstruction that readmits abandoned fork branches (−20 main, −518 subagent). Its safe use is
strictly as a falsifier.

**It never crosses a compaction boundary**, so it can never observe the one place where
`next = prev + assistant + new` is definitely false.

### 1.12d Semantic equality must ignore `cache_control`

Measured on real adjacent capture pairs: **byte equality of the shared prefix fails 4/4; equality
after stripping `cache_control` holds 6/6.** In one pair `messages[0]` is byte-identical at 22,477
bytes while `messages[1]` and `[2]` differ by exactly 48 bytes each — entirely the removal of
`{"ttl":"1h","type":"ephemeral"}`. Breakpoints migrate forward on every call.

v0.8's "recursive semantic equality, not raw byte equality" is therefore **required, not
stylistic**, and the design must state explicitly that the comparison ignores `cache_control` — a
reconstructor cannot place these anyway, since transcripts contain none.

On real wire bodies the invariant **holds**: across a 3-call chain,
`next.messages[0..k] == prev.messages[0..k]` modulo `cache_control`, 100%, with the appended tail
exactly `assistant(prev_response) + new user input`.

### 1.12e What the log does not contain

Not message content — that is complete (§1.3). What is missing is the **injected preamble prepended
to `messages[0]` on the wire**:

- an 8,338-char `<system-reminder>` block wrapping CLAUDE.md, `userEmail` and `currentDate`.
  `grep -c claudeMd` over transcripts returns 0. Subagents receive it too.
- the wrapper text and ordering of agent/skill/deferred-tool listings — the underlying data survives
  as `attachment` records (88.8–98.7% recoverable) but not the rendered form
- `tools[]` mutates between adjacent calls (96 → 116 one turn apart) via deferred loading, recorded
  only as a `deferred_tools_delta` attachment

**Scale matters here and is easy to misread.** The preamble is a fixed ~8 KB (no project CLAUDE.md)
to ~40 KB (a 31 KB project CLAUDE.md). Against a real session carrying 12,027,973 chars of
conversation content that is ~0.3%. A character-weighted "99% not recoverable" figure measured on a
26 KB probe session is not representative.

**The attachment→request mapping is beta-gated and unstable**, which alone defeats transcript-only
list reconstruction. Same machine, same day, two schemes: without the
`mid-conversation-system-2026-04-07` beta the attachments become separate `<system-reminder>`-wrapped
user text blocks *before* the prompt; with it they merge into a single 13,953-char **`role:"system"`
message** placed *after* the user message. The `betas` list is not in the transcript, so a
reconstructor cannot tell which applied. `role:"system"` inside `messages[]` is a fifth message class
the model must accommodate.

### 1.12f Human turns that live only in attachments

**614 genuinely human-typed turns exist only as `queued_command` attachment records** with no
`.message` twin. A reader that walks `.message` alone loses them.

They must be filtered: `queued_command` carries `commandMode` (`prompt` | `task-notification`) and
`origin.kind` (`human` | absent). Of 1,558 unique records only 960 are human input; including the
other 598 in would insert machine-generated `<task-notification>` blobs as if the user had typed them.

### 1.12g Corpus hazards for any absence test

- **28.6% of main-transcript lines carry no `uuid` at all** — 38,340 of 133,887, across 14
  metadata-only record types. None is context-bearing, but a parser assuming every line has a uuid
  will crash or mis-position.
- **177 of 2,935 files contain zero user/assistant/system records** — metadata only. A reconstructor
  treating every `.jsonl` as a session emits 177 empty conversations.
- **The corpus is self-contaminated by this investigation.** Sessions analysing the data wrote the
  strings under test into their own transcripts. A bare `grep` for `cache_control` returns 18 files,
  all of them research prose. Any "no transcript contains X" claim must grep the *field*
  (`"uuid":"…"`) or apply a pre-analysis mtime cutoff.
- **The corpus is live.** Subagent pair counts drifted 56,960 → 57,022 within 20 minutes. Quote
  counts with a timestamp; never treat them as constants.

### 1.13 Two hazards that will bite an implementation

**`sessionId` is not reliably lowercase.** 2 of 47 transcripts use an uppercase UUID
(`41090DAB-113C-…`). Every join must be byte-exact and never case-folded.

**Lines exceed `bufio.Scanner`'s default limit by 15×.** The longest single JSONL line measured is
**979,632 bytes**; the largest file is 59 MB. `bufio.Scanner` silently fails at its 64 KB default
token size. Use `bufio.Reader` with an explicit large buffer, or a streaming JSON decoder.

---

## 2. Raw storage model

### 2.0 Collection posture (decided)

**Claude Code speaks exactly one protocol outward: OTLP.** Exporter selection in the binary accepts
`console|otlp|prometheus` for metrics and `console|otlp` for logs and traces; any other value
throws. There is no webhook, no custom sink, no alternative wire format. Pushing to us means running
an OTLP receiver, or nothing.

Three possible postures:

| Posture | Mechanism | Cost to the user | Yield |
| --- | --- | --- | --- |
| **Pull** | tail `~/.claude/projects/**` | none — Claude Code does not know we exist | all structure and content (§1.2–1.6) |
| **Push** | OTLP receiver (gRPC 4317 / HTTP 4318) | `CLAUDE_CODE_ENABLE_TELEMETRY=1` + `OTEL_EXPORTER_OTLP_*` | measurement only; no agent identity (§1.7) |
| **Exec** | hooks — Claude Code runs our binary, JSON on stdin | edits `settings.json`; on the critical path | `agent_id`, liveness, compaction lifecycle (§1.10) |

**v1 is pull only.** SkyWalking AI Sessionizer faces the filesystem, not the agent. Beyond the channel
reasoning below, one practical argument dominates: **pull works on history that already exists** —
2,900 files and 1.2 GB on the development machine today. Push and exec capture only from the moment
they are configured and can never see yesterday. For a tool whose purpose is reconstructing
conversations, that is a categorical difference.

This also fixes the meaning of `src` in the envelope header: for a pull adapter it is the local
source path, which is not redundant with the layout because the layout deliberately drops the
project slug (we key by session id) and the workflow directory (we flatten children by agent id).
`src` is the only place both survive, and it is what makes `off` verifiable. For a future push
adapter `src` becomes the wire-side origin and `off` drops out.

### 2.0.1 Root location and channel selection (decided)

**`<root>` is assigned by SkyWalking AI Sessionizer configuration.** Default is a directory under the system
temp dir. In tests, the root is placed side by side with the fixtures in the same folder, so a test
case is self-contained and needs no absolute paths.

**v1 collects transcripts only.** Rationale, from §1.7 and the event inventory:

- Every structural edge — stream lineage, tool joins, the spawn chain, epoch boundaries — lives in
  transcripts at higher fidelity than any other channel.
- OTLP **events carry no usable agent identity**: `query_source` is normalized to coarse
  `main | subagent | auxiliary` (the raw `agent:builtin:<name>` string is never exported), and
  `subagent_completed` is emitted with no `agent_id` at all.
- OTLP **spans** would carry `agent_id`/`parent_agent_id`, but require
  `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`, and the two spans that would matter most
  (`subagent.spawn`, `compaction`) are dead code.

What events *do* bring, and why they are a Plan 02+ addition rather than a v1 requirement:
per-call `duration_ms` and `cost_usd` (absent from transcripts entirely), tool `duration_ms` and
`error_type`, and `tool_decision` with a composite `source` (`hook:` / `rule:` / `mode:`).
`assistant_response.message.uuid` is also useful — it is the uuid of the **last** transcript line of
a split message, i.e. the terminal fragment that §1.3 shows is the only authoritative one.

One asymmetry to remember if raw bodies are enabled later: the response file is named
`<request-id>.response.json` and joins to the transcript directly, but the **request** file cannot
be — the request-id does not exist until the response arrives, so it is named by a client-side id,
and `clientRequestId` is absent from transcripts. Joining a request body therefore needs either the
`api_request` event (which carries both `client_request_id` and `request_id`) or the body's own
`.diagnostics.previous_message_id` chain, which works only first-party.

The layout below reserves `log-*` and `trace-*` so adding either channel later requires no rework.

### 2.1 Principles

1. **Session is the primary partition.** Retention, purge, and the pipeline are all session-scoped.
   Purge must be a directory delete.
2. **Files are write-once.** temp name → fsync → rename → `chmod 0444`. Never appended, never rewritten.
3. **The landed file is a buffer, not a wire capture.** OTLP arrives cross-session and must be
   demultiplexed, which means re-serialization. We do not claim byte-exactness with what the client
   sent, and must not imply it.
4. **Loss is detectable.** A monotonic per-session sequence makes a gap visible.
5. **Collection never blocks the agent.**

### 2.2 Layout

```
<root>/<session-id>/
    session.state                              next_seq · liveness · last_scan
    rebuild_progress                           assembler watermark
    streams/
      main/         transcript.cursor · transcript-<ts>-<seq>.jsonl   ExecutionStream E0
      <agent-id>/   transcript.cursor · transcript-<ts>-<seq>.jsonl   E1..En
                    meta.cursor       · meta-<ts>-<seq>.jsonl         agent-*.meta.json
    runs/<run-id>/  journal.cursor    · journal-<ts>-<seq>.jsonl      workflow run journal
                    manifest.cursor   · manifest-<ts>-<seq>.jsonl     workflows/wf_<run-id>.json
                    script.cursor     · script-<ts>-<seq>.jsonl       workflows/scripts/*.js
```

**One directory per workflow run**, not three parallel ones keyed by the same id -
mirroring "everything about one stream in one folder".

**Cursors are named `<kind>.cursor`, not `<kind>-cursor`.** Landed files are
`<kind>-<ts>-<seq>.jsonl`, so a hyphen would make a cursor share a prefix with the data it
tracks and turn any prefix scan into a bug.

**Cursors live beside their stream, not in one session-level file.** A session can hold ~2,700
subagent streams, so a single cursor file would mean rewriting a ~200 KB file for a one-line change
on every pass. Per-stream cursors mean only what changed is written.

`session.state` stays session-scoped because `next_seq` must be monotonic **across all streams** —
that is what collapses `rebuild_progress` to a single watermark. Consequence: **a session is the unit
of work and is processed single-threaded**; parallelism is across sessions, never within one.

**No `origin` file.** The path states the session and stream, the filename
prefix states the kind, and the envelope header (§2.3a) already carries `src`, `session`, `stream`
and `adapter` on every file. A separate metadata file would only restate them. Metadata gets its own
file when content is large enough that finding or indexing it is the problem — which is not the case
for a source path and two ids.

For the same reason `meta.json` is not copied as a raw `.json` blob: it is source content, so it
lands as an ordinary record stream (`kind: agent_meta`, header + one record) and needs no special
read path.

That is the complete v1 tree. Three prefixes are **reserved but not written in v1**, listed here so
adding a channel later needs no restructuring:

| Reserved | Would hold | Blocked on |
| --- | --- | --- |
| `log-<ts>-<seq>.jsonl` | the 25 `claude_code.*` log events, demultiplexed per session, one record per line — the source of per-call `duration_ms` / `cost_usd`, tool durations, and `tool_decision` | §2.0: measurement, not structure |
| `trace-<ts>-<seq>.jsonl` | the 5 reachable spans (`interaction`, `llm_request`, `tool`, `tool.blocked_on_user`, `tool.execution`) | requires `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA` |
| `rawbody/<request-id>.{request,response}.json` | `OTEL_LOG_RAW_API_BODIES` output — the only path to a `full` input manifest | privacy; must be opt-in with redaction |

`<ts>` is receipt time, compact RFC 3339 with nanoseconds (`20260831T091204.481523000Z`) —
sortable, no `:`. `<seq>` is **one monotonic counter per session across all signals**, resumed on
restart from the highest sequence on disk.

**Every stream is a folder, including main.** The main transcript is ExecutionStream E0; giving it
the same shape as the children means the pipeline reads one directory pattern rather than two code
paths, and everything about one stream lives together.

**Child folders are flat by `agent-id`, deliberately not mirroring the source tree.** Two reasons:

1. `agentId` is globally unique (`^a[0-9a-f]{16}$`, 2,686 distinct, zero collisions, never in two
   files), so nesting is not needed for uniqueness.
2. **The storage path must not encode a relationship the pipeline is supposed to derive.** Nesting
   by `wf-id` or by `parentAgentId` would make the tree assert a join; when such a join turns out to
   be wrong — as several did during this investigation — correcting it would mean *moving files*,
   breaking the write-once guarantee. This matters most for the 31 depth-2 agents, whose parent is
   another agent rather than the main stream.

The grouping is preserved as *data*, not as path structure, via three independent routes: the
envelope header's `src` (which retains the full source path including the workflow directory), the
landed `meta` stream (`agentType`, `spawnDepth`, `toolUseId`, `parentAgentId`), and the journal's
`started` records.

**A child's owning session is known two independent ways** — from the source path
(`projects/<slug>/<session-id>/subagents/…`) and from the records themselves, which carry the
parent's `sessionId` (verified 2,686/2,686, zero deviation). The collector must cross-check them at
land time and raise a conflict on mismatch rather than silently preferring one.

Raw bodies are keyed by request-id because the source filename already *is* the request-id, and that
value joins directly to the transcript's `requestId` (§1.8).

### 2.3 Why `.jsonl` and not `.json`

For write-once files the appendability argument is weak, so the real reasons are:

- **Line number is a stable record ordinal.** A source reference becomes *(file, line)* — an
  integer — instead of a JSON pointer into nested structure.
- **Byte offsets are valid resume points.** Our tailer cursor is a byte offset; in a line-delimited
  file an offset always lands on a record boundary.
- **O(1) memory streaming.** Files reach 59 MB; whole-file parse is not viable.
- **One toolchain.** Claude Code's own transcripts are `.jsonl`.
- **A torn write costs one line, not the file.**

For OTLP we therefore flatten each export to **one `LogRecord` or `Span` per line**, with resource
and scope attributes attached, rather than storing the nested `ExportLogsServiceRequest`. The
nesting buys nothing after demultiplexing and costs the ordinal.

`.meta.json` and `rawbody/*.json` stay `.json` — they are single documents at source, not streams.

### 2.3a Uniform record envelope

Every landed **record stream** — regardless of source — uses one shape: a header line carrying
everything invariant for the file, then one line per record carrying only what varies.

```jsonl
{"h":1,"seq":175,"at":"2026-08-31T09:12:06.114000000Z","kind":"transcript",
 "adapter":"claude-code/0.1.0",
 "src":"-Users-…-python/92a5d58e-…/subagents/agent-afdc6064f2f8dcb06.jsonl",
 "session":"92a5d58e-36cf-4838-a58c-66058206b903","stream":"afdc6064f2f8dcb06",
 "state":"available","derivation":null}
{"ord":1042,"off":532231,"sha":"9f3a1c2b…","payload":{…}}
{"ord":1043,"off":533104,"sha":"1b2c4d5e…","payload":{…}}
```

**Source kinds.** Note that `log` and `event` are not two sources: Claude Code's events *are* OTLP
log records, with body = the string `claude_code.<event_name>`.

```
transcript | otlp_span | otlp_log | provider_body | agent_meta | journal | derived
```

`derived` is SkyWalking AI Sessionizer's own output; the slot is reserved now so Plan 02 needs no format
change.

**Why header + records.** `kind`, `src`, `session`, `stream` are constant for a whole delta file.
Repeating them per record costs ~200 bytes × ~350k records ≈ 70 MB of pure redundancy. Hoisting them
also makes each file self-describing — it stays interpretable if moved out of its directory, which a
bare stream of source lines is not. For OTLP the header pays off twice: resource and scope
attributes are per-export constants and belong there rather than on every record.

**Per-record fields.**

| Field | Purpose |
| --- | --- |
| `ord` | ordinal **in the source**, not in this file — deltas start mid-file, so these differ, and only `ord` is stable across passes |
| `off` | byte offset in source; lets any record be re-verified against the original while it exists |
| `sha` | digest of the payload bytes |
| `payload` | the source record, **byte-for-byte** |

**Byte-fidelity is preserved.** The payload is copied through as raw bytes rather than re-serialized
(`json.RawMessage` in Go), so key order, whitespace and number precision all survive. The envelope
does not cost us the exactness of the wrapped record.

**Per-source mapping.**

| Source | `kind` | `ord` | `off` | Notes |
| --- | --- | --- | --- | --- |
| main / subagent transcript | `transcript` | source line no. | byte offset | payload verbatim |
| workflow journal | `journal` | source line no. | byte offset | payload verbatim |
| `agent-*.meta.json` | `agent_meta` | 0 | — | single document: header + 1 record |
| OTLP logs *(reserved)* | `otlp_log` | index in export | — | `derivation:"otlp_flatten"`; resource+scope in header |
| OTLP spans *(reserved)* | `otlp_span` | index in export | — | same |
| raw bodies *(reserved)* | `provider_body` | 0 | — | header + 1 record |
| sessionizer output | `derived` | — | — | Plan 02+ |

`state` is the record-level content state (`available | redacted | truncated | omitted |
unavailable`). In v1 nothing is redacted at collection time, so it is `available` in the header and
never overridden; the slot exists for privacy modes later. Field-level redaction — such as the
stripped thinking text in §1.3 — is a coverage concern for Plan 02, not a collection-time one.

**State files are not record streams.** `cursor`, `session.state` and `rebuild_progress` stay
plain line-oriented key-value text (§2.5). The rule is: streams get envelopes, state files stay
simple.

### 2.4 Sequence, duplicates, and what digests can honestly prove

Protobuf serialization is **not canonical** — the spec does not guarantee byte-identical output for
the same logical message. "Same content ⇒ same bytes" is false in general.

What *is* true is narrower: an OTLP exporter retrying a failed export re-sends the same serialized
buffer. That is a property of exporter behaviour, not of protobuf.

Therefore:

- **Drop duplicates at the receiver's edge**, in memory, on the digest of received wire bytes *before*
  demultiplexing. A retry hits the cache and is dropped.
- **No digests in filenames for duplicate checks.** After demultiplexing the bytes are ours; a digest attests
  to our buffer's integrity, not to what the client sent.
- **The assembler must tolerate duplicate records regardless** — an intermediate Collector can
  re-batch, delivering the same record inside two payloads no digest will match. It must also drop duplicates
  transcript records by `(file, uuid)` for the independent reason in §1.2.

Sequence gaps detect *our* losses. They cannot detect client-side drops, because OTLP carries no
client sequence number. That should be reported as unknown, not papered over.

### 2.5 State files

All are plain line-oriented text, rewritten atomically via temp-and-rename, holding current state
rather than history. If an audit trail is wanted later, that is a separate append-only log.

#### Two cursor kinds

The six source kinds (§2.8) divide into growing files and rewritten files, and they need different
cursors.

**Append cursor** — for `.jsonl` sources that only grow:

```
# streams/afdc6064f2f8dcb06/cursor
schema        1
kind          append
source        subagents/agent-afdc6064f2f8dcb06.jsonl   # relative to source_root
dev           16777232
ino           8461299
offset        532231        # bytes consumed → next read starts here
ord           1042          # source line consumed through → next batch starts at ord 1043
last_uuid     5acb6271-ea30-4e55-a27f-ff8a41f579c0
tail_sha256   1a2b3c…       # sha256 of [offset-1MiB, offset)
last_seq      175
size_at_scan  532231
updated_at    2026-08-31T12:00:00.000000000Z
state         active        # active | source_gone | conflict
```

Each field earns its place against a specific failure:

| Field | Catches |
| --- | --- |
| `dev` + `ino` | the file was replaced or rotated under the same path |
| `offset` vs `size` | truncation, and "is there anything new" without trusting mtime |
| `tail_sha256` | rewrite of recently-consumed bytes |
| `ord` | avoids re-counting lines from byte 0 to continue numbering |
| `last_uuid` | semantic drift — **diagnostic only** (28.6% of lines carry no uuid, §1.12g) |
| `state` | distinguishes a pruned source (normal) from a conflict (stop) |

`offset` is both "position consumed" and "next round's starting point" — one value, no second field.
They diverge only for the in-flight tail: if the file is 90,000 bytes but the last complete newline
is at 85,820, we record 85,820. Recording what was *scanned* rather than *consumed* would be wrong.

**Why a tail window rather than a full prefix digest.** Re-hashing `[0, offset)` every pass means
re-reading up to 59 MB per cycle. The window is sized at 1 MiB, comfortably above the largest
observed line (979,632 bytes), so it always spans at least one complete record boundary. Combined
with `ino` (rotation) and `size < offset` (truncation), the only uncovered case is an in-place edit
far behind the cursor — not a Claude Code behaviour. Full-prefix hashing becomes an opt-in strict
mode.

**Snapshot cursor** — for whole-file JSON that is *rewritten*, not appended:

```
# manifest/wf_0477d6c4-5e3/cursor
schema          1
kind            snapshot
source          workflows/wf_0477d6c4-5e3.json
content_sha256  9f3a1c2b…
size            18422
mtime           1788046628
last_seq        176
updated_at      2026-08-31T12:00:00.000000000Z
state           active
```

A snapshot cursor lands a **new immutable version whenever the digest changes**, turning a mutable
source into an immutable version history. `wf_<run-id>.json` genuinely needs this — it carries
terminal state (`completed`, `killed`), `durationMs`, `totalTokens` and `result`, so it is rewritten
as a run progresses. `agent-<id>.meta.json` uses the same cursor but in practice lands exactly once
(§2.8).

#### `rebuild_progress`

Assembler state. Because `<seq>` is monotonic **across all streams**, it collapses to one watermark
regardless of how many child streams exist:

```
updated_at  2026-08-31T09:14:02Z
pass        41
landed_seq  174
```

That is the point of the shared counter — without it this file would need thousands of per-stream
lines. It is safe because the collector assigns the sequence at write time, so a file never appears
later with a lower sequence and the watermark cannot skip a hole.

### 2.6 The mutable-source problem

Transcripts are appended to **live**, so they cannot be referenced in place — they violate the
write-once model everything else depends on.

Proposal: **tail them into immutable deltas.** Each `transcript-<ts>-<seq>.jsonl` contains only the
bytes appended since the last cursor. Mutability is then confined to the tailer.

Evidence supports this strongly: resume appends to the same file, and across all 60 main + 2,851
subagent files there are **zero dangling parentUuids and zero cross-file references** (§1.6). The
replay phenomenon (§1.2) is also an *append* — a contiguous block written at the resume point — so
the file stays append-only and the prefix digest holds. Whether truncation ever occurs is unproven
(§1.9), which is exactly what the prefix-digest check exists to catch.

Open question: continuous tailing (a daemon) versus on-demand scanning at rebuild time. On-demand is
simpler and loses nothing **provided** transcripts are never truncated. The §1.9 item decides this.

---

### 2.7 Pull-mode positioning and completeness (Phase 1)

Positioning is a **pull-only** concern. Phase 2 (push) has no cursor — it has an at-least-once
delivery problem instead. Different failure modes, which is the main reason the two postures should
not share an abstraction beyond "produces landed records".

**A session is never "complete".** Resume appends to the *same* file with the *same* sessionId —
proven by a 659-hour gap in one transcript where `version` jumped 2.1.220 → 2.1.251 and the chain
still linked (§1.6). A file quiet for a month can grow again. The collector can therefore never mark
a session done and stop watching; it can only report **quiet since T**. This propagates: retention
cannot trigger on "completed", and segment closing needs the `close_candidate` → gates → `committed`
staging of v0.8 §3.3 rather than a bare idle timeout.

Five levels of completeness, each with its own rule:

| Level | Rule |
| --- | --- |
| **Byte** | consume only up to the last `\n`; a trailing partial line is an in-flight write — never parse it, never advance past it |
| **Record** | free once byte-level holds (this is why `.jsonl`, §2.3) |
| **Provider call** | close call C when a line with a different `message.id` arrives. **Do not wait for the terminal fragment** — 7.14% of calls never get one (§1.3), so waiting would hang on one call in seven. Mark `terminal_missing` instead. |
| **Stream** | a subagent is finished per the journal `result`/`failed`, the `<task-notification>` status, or the parent's tool_result — but **the journal leads the filesystem** (§1.5), so "told it finished, haven't seen its data yet" is a normal state |
| **Session** | no session-end record exists. The only liveness signal is `~/.claude/sessions/<pid>.json`, present while the process runs and deleted on exit. State model is `live \| quiet \| unknown` — never `complete`. |

**Cursor shape** — four fields, each catching a different failure:

```
src  <path>  dev=16777232 ino=8461299 offset=8823104
             uuid=ccb3597f-… sha256=1a2b3c…
```

- `offset` — resume point, always on a line boundary
- `dev`+`ino` — catches **rotation**: same path, different inode means replaced, not appended
- `uuid` — cheap semantic anchor; the last record consumed should still be there
- `sha256` — digest of bytes `[0, offset)`, the authoritative rewrite check

Re-hashing 59 MB every pass is wasteful. Check the cheap invariants first — `size >= offset`, inode
unchanged — and do a full re-verify only when one trips. Go's `sha256` implements
`encoding.BinaryMarshaler`, so the rolling state can be persisted and extended rather than
recomputed in the forward direction.

### 2.8 Pull-mode collection process (Phase 1)

#### The seven source kinds

| # | Source | Nature | Cursor |
| --- | --- | --- | --- |
| 1 | `<session-id>.jsonl` | append-only | append |
| 2 | `subagents/agent-<id>.jsonl` | append-only | append |
| 3 | `subagents/agent-<id>.meta.json` | write-once | snapshot |
| 4 | `subagents/workflows/<wf>/agent-<id>.jsonl` | append-only | append |
| 5 | `subagents/workflows/<wf>/journal.jsonl` | append-only | append |
| 6 | `workflows/wf_<run-id>.json` | **rewritten** | snapshot |
| 7 | `workflows/scripts/<label>-wf_<run-id>.js` | rewritten | snapshot |

**Workflow scripts carry their run id in the filename**, which is what makes them
collectable despite living outside their session's directory:

```
<label>-wf_<run-id>.js     session from the path, run id from the name
```

Split on the **LAST** `-wf_`: a run id contains hyphens (`wf_afa31e47-f6c`), so a greedy
match starts at the first occurrence and swallows any later one. Verified on a real corpus:
172/172 filenames parse, and 172/172 run ids resolve to a manifest in their own session.

A script is JavaScript, not JSON, so it lands with `state: "raw"` - wrapped as a JSON string
so the landed file stays parseable, with the bytes recoverable exactly.

**`meta.json` is write-once.** 200 of 200 sampled meta files are *older* than their own
`agent-<id>.jsonl` — zero equal, zero newer. Its content is spawn-time only (`agentType`,
`description`, `spawnDepth`, `toolUseId`, `parentAgentId`, `model`); no status, result or duration.
There is **no registry file** — each agent owns its own sidecar, so nothing is ever removed from a
list. Pairing is exact: 2,697 meta ↔ 2,697 jsonl, zero orphans either direction.

**`journal.jsonl`** records `{"type":"started","key":"v2:<sha>","agentId":…}` and `{"type":"result",…}`
— 389 started / 347 result in the sample, the shortfall being in-flight and killed agents. It is
**workflow-only**; Agent-tool spawns and Skill-forks have no journal. And it **leads the filesystem**:
a `started` — occasionally even a `result` — appears before the child transcript exists. Normal, not
a gap.

**No single kind carries spawn lineage.** Of 300 sampled meta files, 210 hold only
`{agentType, spawnDepth}` — **70% of children carry no parent pointer in their own sidecar**, because
they are workflow subagents. The link is distributed:

| Mechanism | Parent side | Child side |
| --- | --- | --- |
| Agent tool | main: `tool_use{name:"Agent"}` + `toolUseResult.agentId` | `meta.json.toolUseId` — redundant, so cross-checkable |
| Skill fork | main: `toolUseResult.agentId`, `status:"forked"` | **nothing** |
| Workflow | main: `Workflow` result → `toolUseResult.transcriptDir` + `runId` | `journal.jsonl` `started` records |
| Depth 2 | — | `meta.json.parentAgentId` |

So all six kinds are collected. The main transcript alone cannot enumerate workflow children; the
child files alone cannot name the parent for 70% of children.

#### Discovery is session-first, not project-first

**A session's files scatter across several project directories.** Measured: **7 of 61 sessions span
more than one slug**, one of them across six.

The cause is **workflow scripts, not execution streams.** Claude Code files a run's script under
whatever working directory the agent had at the time; the conversation data - main transcript, child
streams, journals, manifests - stays in one directory. Before scripts were collected, those extra
directories yielded nothing and were correctly not recorded. Now that scripts are collected the
directories are real again, which is why the exclusion matcher keys on the session's PRIMARY
directory: a script filed elsewhere must not disqualify the conversation it belongs to.

```
f9d8df3b-c47b-4f56-829c-37f4d81486cf
   transcript  -Users-wusheng-github-skywalking-horizon-ui
   dir         -Users-wusheng-github-skywalking-horizon-ui
   dir         -Users-wusheng-github-skywalking-horizon-ui-test-e2e
   dir         -Users-wusheng-github-skywalking-horizon-ui-apps-bff-src
   dir         -Users-wusheng-github-skywalking-horizon-ui-apps-bff-src-store-audit
   dir         -Users-wusheng-github-skywalking-horizon-ui-apps-bff-src-client-banyandb
```

Cause: a subagent running in a different working directory is filed under *that* cwd's slug, not the
session's launch slug — the `cwd`-variance of §1.2 made visible in the filesystem.

**Rule:** scan every project directory for UUID-shaped entries — both `.jsonl` files and directories —
then **group by session id across slugs and union them**. A project-first walk misses most subagents
for 11% of sessions.

This also makes filters session-scoped: excluding a slug must not cut streams out of a session
being collected, so `include`/`exclude` are evaluated as "does any part of this session match", never
per-directory.

Two further discovery rules from the same scan:

- **Filter directory names by UUID shape.** `memory/` is a sibling directory inside project
  directories and is **not** a session. It is excluded.
- **Pruning is real and not atomic.** 12 of 41 session directories have no main transcript — the
  `<session>.jsonl` was deleted while the whole `subagents/` tree survived. Expect child streams with
  no parent. Separately, 32 of 61 main transcripts have no subagent directory at all, which is simply
  a session that spawned no agents.

#### Directory names are slugified cwd, and irreversible

`/Users/wusheng/github/skywalking-horizon-ui` → `-Users-wusheng-github-skywalking-horizon-ui`.
Verified across all 25 directories carrying a transcript; two mismatches revealed the full rule —
`/`, `.` **and space** all collapse to `-`:

```
/Users/…/Application Support/JetBrains/GoLand2026.1/projects/…
  → -Users-…-Application-Support-JetBrains-GoLand2026-1-projects-…
/private/tmp/agentsessionizer-cc-v04.psixvf
  → -private-tmp-agentsessionizer-cc-v04-psixvf
```

So a slug cannot be reversed to a path. **Forward slugification is deterministic; reverse is not** —
which is why config accepts real paths and slugifies them to match, and never attempts the inverse.
The real `cwd` is recoverable from record content, but it varies within a session (one session showed
10,379 records at the project root plus Downloads, a scratchpad and several subdirectories), so it is
not a session-level key.

#### Configuration

```yaml
storage:
  root: ./data                    # beside the binary; tests point it beside their fixtures

adapters:
  - name: claude-code-local
    enabled: true
    # source_root resolution: this value → $CLAUDE_CONFIG_DIR/projects
    #                       → $XDG_CONFIG_HOME/claude/projects → ~/.claude/projects
    source_root: ""
    # Entries starting with "/" are real paths, slugified forward for matching.
    # Anything else is a glob against the directory name. Evaluated per SESSION.
    include: []                   # empty = all
    exclude:
      - "/private/tmp/**"         # Claude Code's own agent scratchpads
    collector:
      mode: watch                 # watch | once
      interval: 5s
      max_delta_bytes: 8MiB
```

`claude-code-local` names both the runtime and the collection posture, leaving room for a future
`claude-code-otlp` reading the same runtime by push. `collector` is an adapter attribute, not a
global section, because cadence is a property of how a given adapter reaches its source.

`mode: once` matters as much as `watch`: the first run over existing history is a backfill of ~1.2 GB
across ~2,900 files, not a tail. Same code path, different exit condition.

A `sources list` command is needed so nobody has to guess at slugs — printing slug, resolved `cwd`
(from record content) and session count.

#### The consuming workflow

```
┌─ DISCOVER (session-first) ────────────────────────────────────────┐
│ walk all project dirs → UUID-shaped .jsonl files AND directories  │
│ group by session id ACROSS slugs → union                          │
│ skip non-UUID dirs (memory/)                                      │
│ liveness: does ~/.claude/sessions/*.json name this session?       │
│ CHANGE TEST: append → stat().size vs cursor.offset                │
│              snapshot → content digest vs cursor.content_sha256   │
└───────────────────────────┬───────────────────────────────────────┘
                            │ sessions with ≥1 changed source
┌─ PER SESSION (single-threaded) ───────────────────────────────────┐
│                                                                   │
│  VALIDATE   ino ≠ cursor.ino     → rotation   → conflict, stop    │
│             size < cursor.offset → truncation → conflict, stop    │
│             file gone            → source_gone (NOT an error)     │
│             else verify tail_sha256                               │
│      ▼                                                            │
│  TAIL       seek(offset); read to the LAST COMPLETE '\n'          │
│             bytes past it = in-flight write → leave them          │
│             unparseable line mid-stream → conflict                │
│      ▼                                                            │
│  LAND       seq = session.state.next_seq++                        │
│             write .tmp: header + records (payload verbatim)       │
│             fsync → rename → chmod 0444                           │
│      ▼                                                            │
│  COMMIT     cursor → tmp → rename                                 │
└───────────────────────────────────────────────────────────────────┘
```

**Land before commit — deliberately at-least-once.** A crash between the two re-lands the same
records next pass. That is safe *for free*, because the assembler must already drop duplicates by
`(file, uuid)` for an unrelated reason (§1.2 — Claude Code itself writes duplicates). The reverse
order would lose data. No write-ahead log is needed.

**Change detection is `size` vs `offset`, never mtime.** One `stat()` per file; ~2,900 stats is
milliseconds, and it needs no assumption about clock skew or timestamp granularity.

**Source deletion is normal.** 330 of 345 session ids in `history.jsonl` have no surviving
transcript. The cursor moves to `state=source_gone` and the landed deltas remain — so the landing
zone outlives the source, and retains conversations Claude Code has already discarded.

**Files appearing mid-pass is normal**, because the workflow journal leads the filesystem. Each pass
re-discovers rather than caching a file list.

#### Open: conflict recovery

Rotation or a tail-digest mismatch means the source changed underneath us. Stopping and flagging is
right; what follows is not settled. Proposed default: **re-land from offset 0 into a new stream
generation** (`streams/<id>@2/`), so nothing is lost and nothing is silently merged. Evidence says
this should never fire — transcripts look strictly append-only — but §1.9 lists that as unproven.

## 3. Deferred

**Phase 1 (local logs) — remaining plans:**

- **Plan 02 — per-session pipeline:** duplicate uuids removed first, then fragment reassembly by
  `(file, message.id)` **in line order, never by ancestor walk** (§1.12a), stream partitioning, the
  three spawn joins, epoch boundaries via `logicalParentUuid`, segment gates, continuity audit
  scoped as a structural cross-check (§1.12c).
- **Plan 03 — output:** conversation bundle, export projection, preview.

**Phase 2 (OTLP) — additive, no Phase 1 rework:**

- OTLP receiver, per-record demultiplex by `session.id`, landing into the reserved `log-` / `trace-`
  prefixes (§2.2).
- Measurement only: per-call `duration_ms` and `cost_usd`, tool durations, `tool_decision`.
  No structural contribution (§1.7).
- Metrics are a third, separate pipeline with its own retention. Nothing in metrics contributes to
  rebuilding a conversation.

**Orthogonal opt-in, either phase:**

- Raw provider bodies. The only route to a verified `input_manifest` and the only way to check the
  continuity invariant against what was actually sent (§1.8, §1.12d). Highest privacy risk in the
  system; must be opt-in with redaction.

---

## 4. Open questions for discussion

1. **Does the collector own an OTLP endpoint, or read files only?** Given §1.7 — no `agent_id` on
   events, and lineage spans unreachable without a beta flag — the OTLP channel now looks
   substantially *weaker* than transcripts for structure. It contributes timing and cross-validation.
   This weakens the case for a receiver in v1.
2. **Should hooks be the second channel instead of OTLP?** §1.10 says hooks carry `agent_id` on
   every subagent hook, name the child transcript directly, and expose the full compaction lifecycle
   — everything the unreachable spans would have given us, without a beta flag. Cost: they are on
   the critical path and require editing the user's `settings.json`.
3. **Is `OTEL_LOG_RAW_API_BODIES` in scope for v1?** It makes exact model-input membership reachable
   and needs no proxy. It is also the highest-privacy-risk surface in the system and must be opt-in
   with redaction.
4. **Where does `<root>` live** — under `~/.claude/`, alongside it, or a configured path?
5. **Retention:** per-session TTL, or unbounded until explicitly purged?
