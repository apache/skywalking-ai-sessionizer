# Design Plan 02 — Parse and Assembly

**Status:** implemented. The pipeline is `internal/assemble`, the round chain is `pkg/sessionflow`, the
round driver is `internal/parse`, and `asz parse` / `asz conversation` drive them.
**Scope:** turning landed records into a conversation structure. Collection is Plan 01 and is
implemented; export and preview are Plan 03 and are not designed.

Every rule below is forced by a measurement. Where a rule looks arbitrary, the evidence that forced
it is cited, because several of them are the opposite of the obvious choice.

Numbers marked **(corpus)** were re-measured over the whole corpus — 2,970 files, 1.09 GB, 365,825
records, 25 project directories — and each was then re-derived independently by a second pass that
was told to refute it. Where the two disagreed, the number below is the one that survived. Several
claims in the first draft did not.

---

## 0. What parse consumes

Parse reads the **index**, not the payloads.

```
365,554 index entries    describing 1.0 GB of landed payload
47 MB total index        ~12x smaller
16 MB / 4.5 ms           largest single session, load + rebuild every lookup map
```

Structure resolution needs identifiers, not content: removing duplicate records needs record ids, provider-call
grouping needs message ids, tool joins need tool-use ids, spawn joins need agent and run ids. None
of it reads message text. Payloads are read only in the final materialisation step, and only for
records that appear in the output.

The index is derived and disposable; deleting it rebuilds from landed files.

---

## 1. The pipeline

Eight stages. The order is forced: each depends on the one before, and two of them are wrong in the
obvious formulation.

### 1.1 Remove duplicate records, keeping the first

**Nothing downstream is correct before this.** Record ids are not unique within a source file.

**(corpus)** 1,533 repeated ids among 318,199 distinct ids across 2,970 files. **Multiplicity is
exactly 2, never 3**, so a single pass is provably enough — no iteration, no fixed point. They occur
in 11 of 62 main transcripts and in **0 of 2,908 subagent transcripts**.

**They are not a resume.** The first draft said they were. In 11 of 11 files the repeated run ends on
the record immediately before a `system`/`compact_boundary` — every time, with no exception. This is
the runtime re-writing a block of history just before it resets model context. It matters
operationally: duplicate removal and epoch cutting touch the same lines, so **removal must run
first**, and the epoch cut must not read the re-written block as new content.

**First occurrence wins, and the reason is data, not order.** The later copy is the worse one:

- `promptId` is rewritten on 538 of 1,533 pairs, collapsing onto the cycle that was active at reset
  time. Keeping the last would attribute those records to the wrong turn.
- `toolUseResult` is present on one side of 536 pairs, and the first copy is larger in 430 of them
  and smaller in **zero**. 43.4% of captured tool output would be discarded, and in 314 pairs the
  later copy has `stdout` blanked to `""`.
- `parentUuid` differs on 40 pairs, with both parents present in the file, so keeping the last
  changes the graph that stages 1.3 and 1.7 walk.

The `message` object itself is byte-identical in 1,533 of 1,533 pairs. Only the envelope differs —
which is exactly why this looks harmless and is not.

**Nothing distinguishes an original from a copy except line order.** Timestamps are identical
1,533/1,533, message ids identical where present, block ids identical. So "first" has to mean first
in file order; there is no content test to fall back on.

**44,585 of 365,825 lines carry no record id at all** — sidecar types keyed only by session. A pass
that indexes by id must not treat a missing id as one colliding key. `internal/index` skips them:
an entry with no identity is always its own record.

> The index applies all of this. `Canonical()` is the deduplicated view, and a repeated copy is
> excluded from **every** derived lookup, not only from id resolution.

### 1.2 Partition streams

`main` and one stream per agent id, already separated by the landing layout. A child stream **always
shares the parent's session** — **(corpus)** 2,908 subagent files, zero exceptions, and no file
carries more than one session id.

**Stream membership comes from the file, never from a field.** `isSidechain` looks like the
discriminator and is not: it is absent on 28.7% of main records, so a truthiness test misclassifies
nearly a third of the parent lineage. Inside a subagent file it is a constant, so it carries no
per-record information there either. It is not indexed.

**(corpus)** Nesting stops at depth 2: 2,683 sidecars at depth 1, 31 at depth 2, one with the field
absent — which must default to 1 rather than fail. The layout carries **no** depth information; all
2,715 agent transcripts sit flat in two directories. Depth is only in the sidecar. All 31 depth-2
agents come from one session, so any "sample N sessions" method has a good chance of seeing none and
concluding wrongly that the maximum is 1.

**A recursive walk is required.** A non-recursive listing of `subagents/` finds 107 files where a
recursive walk finds 2,910 — a 96.3% miss, because most children live under
`subagents/workflows/<run-id>/`.

### 1.3 Assemble provider calls by `(file, message.id)` in **line order**

**Never by walking `parentUuid` forward.** The graph is a DAG, not a chain: parallel tool dispatch
forks it, so a forward walk traverses one branch and silently drops the other `tool_use` and its
result.

The ban is on the **direction**, not on the edge. Walking *backward* - from a record to its nearest
ancestor carrying a given field - is single-valued and correct, because a record has exactly one
containment parent. Stage 1.7 depends on that. Only forward traversal is ambiguous.

**(corpus)** The forward walk drops a `tool_use` on 16,063 of 85,057 tool-bearing calls — 18.9%
pooled, 2.1% on the parent lineage, 25.9% on child streams. The first draft explained this as
parallel dispatch forking the graph. That is wrong: no fragment ever has two children sharing its
message id, so there is no branch to choose. The real reason is that a call's fragments are
**interleaved with their own tool results**, in the parent chain and in line order alike — 24.0% of
multi-fragment calls have foreign records inside their line span. So grouping must be a full scan
keyed by message id, and must not assume a call occupies a contiguous run of anything.

Group by `message.id`, **not** `requestId`: a `<synthetic>` companion record can reuse a real call's
`requestId`, which would put fabricated content inside a real call.

Synthetic filter: `message.model == "<synthetic>"`. Not `requestId == null` — 24 of 97 synthetics
carry a non-null `requestId`, and 2 genuine calls have none.

**Usage: take the last fragment in LINE ORDER. Never sum, and never use the stop reason to find it.**

The stop reason is not a terminal marker. **(corpus)** a main transcript stamps the same stop reason
on every fragment of a call (23,324 of 23,329 groups); a child stream stamps it only on the last. A
stop-reason test therefore picks the first fragment on main and the last on a child. Line order is
the only rule that works on both.

Summing is wrong for a different reason on each side. Main repeats the call's final usage on every
fragment, so adding them multiplies the real number by the fragment count — **(corpus)** 2.18x
pooled, 9.0x worst case. Child streams write streaming partials that climb 1, 2, 3, where the last
value is the whole count and summing adds only 2%.

**(corpus)** ~10% of calls never report a stop reason (0.02% main, 14.5% subagent). Two traps here:

- Their usage block is **present and wrong** — output counts of 1 to 5, a streaming stub. 5,739 of
  7,410 such calls have a last value of 5 or less, against 101 of 66,751 finished ones. So usage
  availability is gated on the stop reason, never on the field being present.
- They are **not in flight**. Only 72 of 7,410 sit at the end of a file; the rest are mid-file with
  the conversation continuing past them, and 7,311 had their tools actually run. Nothing waits.

Fragments per call: **(corpus)** 2.09 pooled, ranging 1.59 to 2.51 per session. Treating records as
calls inflates every count by that factor.

Synthetic records have a further signature: **(corpus)** all 122 have a UUID-shaped message id while
all 184,330 real ones are prefixed `msg_`, with no overlap. So a fabricated record can never collide
with a real call's id.

### 1.4 Join tools

`tool_use_id` → the index's tool lookup. **(corpus)** every `(file, tool_use_id)` maps to exactly one
result once duplicates are removed. `tool_use` → `tool_result` is strictly one to one; a provider
call to its tools is one to many, up to 16.

**There is a second, fully redundant join the first draft missed.** Every tool result carries a
top-level `sourceToolAssistantUUID`, and it matched the record holding the corresponding `tool_use`
in **105,409 of 105,409 cases** — a direct record-to-record edge, and a free integrity check on the
id join.

**`srvtoolu_` is not a tool-use id.** The first draft said it was a second prefix to accept. It is an
id *inside a WebSearch result payload*, one level below the tool call, and the assembler never joins
on it. Nothing may match on a prefix either way.

**Absence of `is_error` does not mean success** — it is present on 85.5% of result blocks.

**`toolUseResult` is a parent-lineage field.** Present on 100% of main-transcript tool results and
2.1% of child-stream ones, so everything read from it — captured output, and the spawn joins in §1.5
— is implicitly a parent-lineage mechanism.

**It is not always an object.** On roughly 7% of results it is a bare string. Decoding it into a
struct fails, and because the failure covers the whole record, every identifier on that record is
lost with it. It must be read as raw JSON and shape-checked.

Execution timing is reported `unavailable`. "No local record" is slightly too strong: three tools do
give a real duration (`WebFetch.durationMs`, `WebSearch.durationSeconds`, `Agent.totalDurationMs`)
and could populate it for exactly those. A configured budget such as a timeout is not a measurement,
and a timestamp delta includes model and harness latency, so neither is used.

### 1.5 Join child agents to the calls that started them

No single field spans them. The first draft ordered these by how obvious they look; the corpus
orders them by how much work they do, and the two orders are almost reversed.

| Mechanism | Join | Share **(corpus)** |
| --- | --- | --- |
| **Workflow** | the launch result's `runId`, plus `journal.jsonl` `started` records naming each child | ~96% |
| **Agent tool** | the parent's `toolUseResult.agentId` | |
| **Skill fork** | the same field with `status:"forked"`; there is no separate pointer to the call, so the tool id comes from the result's own content block | |
| **Nesting** | the child sidecar's `parentAgentId` | 31 children |
| | *together* | ~4% |

**The sidecar is not a general parent pointer.** The first draft said 70% of children lack one; the
real figure is worse and the conclusion different. A sidecar normally holds only `{agentType,
spawnDepth}` — nothing about who started it. It names a parent for a *nested* child and nothing else,
so it is a 3.3% edge case, not a primary join. Without `parentAgentId` a child of a child is
attributed to the main stream, flattening all 31 depth-2 spawns.

**The journal answers one question only:** which children belong to a run. It carries no timestamp,
no name, no parent pointer and no tool id — just an agent id and a content hash. So the launch
itself still has to come from the parent's own result, which names the run. That is how a workflow
spawn becomes an edge between the call and the child, with no invented node standing for the run.

**`status` has three values, not two.** `async_launched` is a launch acknowledgement and not a
result. `forked` is a skill fork. `completed` is a **synchronous return, and it IS the result** — the
first draft did not mention it, and treating it as an acknowledgement loses a real output.

Completion arrives as a notification, and its two ids are not equally good. **`<task-id>` is a
generic handle** whose namespace depends on what was launched — an agent id only ~8% of the time,
otherwise a background-command id, a workflow task id, or a monitor id. Joining on it
unconditionally matches the wrong thing most of the time. `<tool-use-id>` is exact and is the primary
key; the call it names already has a spawn edge, so resolving through the call works wherever the
task id does not. A single notification can also carry **several** `<task-id>` elements.

Two further cautions hold as written. The `<tool-use-id>` is **not always resolvable in the file it
lands in** — a nested child notifies the main lineage while the call that started it lives in an
agent file — so resolution searches the whole session. And `<status>` is **absent entirely** on a
large class of notifications, so nothing may require it.

**A child with no parent at all exists.** 99.93% resolve; two in the corpus do not. That is a state
to record, not an assumption to assert — an orphan stream stays attached to the session and to no
Talk, because guessing a parent is worse than saying none was found.

**`agent.output` ownership is a policy, not an observation.** The first draft read as if the parent
simply did not receive the child's output. It does: a `status:"completed"` result and a notification
both carry a copy. The rule is that we *deliberately drop* the parent's copy and keep a reference,
because otherwise a rendered conversation repeats every subagent's work inside its parent. The index
marks those records so a renderer knows there is something to drop.

`agent.output` belongs to the **child** stream. The parent gets a compact boundary that references
it, never absorbs it — this is what stops a rendered conversation duplicating every subagent's work.

### 1.6 Cut context epochs

`system/compact_boundary` carries **`logicalParentUuid`**, an explicit pointer to the last message
before the reset. **(corpus)** present on 27 of 27 boundaries and on no other record type anywhere.
Epochs are never inferred.

- **the boundary re-roots the chain.** Its own `parentUuid` is null on 27 of 27, so
  `logicalParentUuid` is the *only* link back. Anything walking parents across a reset stops dead
  without it.
- boundary `uuid` == the `parentUuid` of the following `isCompactSummary:true` record, so the pair is
  joined by containment
- the summary's timestamp is **always earlier** than its boundary's, by up to ~1.4 s — so an epoch is
  ordered by landed position and never by time
- `logicalParentUuid` is **never** the immediately preceding line, so proximity cannot substitute
- `preservedMessages.allUuids` may name records that exist nowhere on disk, so both it and
  `logicalParentUuid` have to be unresolved-capable
- the summary carries a `promptId` but **no `origin`** — which is exactly the case §1.7 has to handle
  without an explicit trigger
- child streams never reset — **(corpus)** 0 in 221,592 child records, so every child stream is
  exactly one epoch
- searching for the literal text `compact_boundary` is unsafe: it appears far more often in message
  content than in real boundary records

### 1.7 Build Talks and Runs

This is the stage that produces the conversation a person reads, and the one with the least
runtime support: neither Talk nor Run is a Claude Code concept. Both are derived, so the boundary
rule has to be stated exactly.

**Two runtime signals map onto one model concept. Conflating them is the trap.**

`promptId` and `origin` are Claude Code's own undocumented fields. Neither is a model concept and
neither may appear outside this adapter; the model has Talk, and nothing below it.

| Side | | Meaning | Sampled session |
| --- | --- | --- | --- |
| runtime | `promptId` | one *prompt cycle* | 65 |
| runtime | `origin.kind` | what triggered that cycle: `human` or `task-notification` | 36 / 29 |
| **model** | **Talk** | a human trigger plus the notification cycles it caused | **36** |

The identity is close but not exact:

```text
distinct promptId  ==  count(origin.kind=="human")  +  count(origin.kind=="task-notification")  +  cycles with no origin at all
```

The third term is the correction. **A local slash command is a person acting and the runtime records
no origin for it**, so a rule that required an explicit human marker would leave those cycles
attached to whatever came before. The rule is therefore:

> **A Talk starts on an external trigger, OR on a cycle where no record states a trigger at all.**
> Everything else continues the interaction in progress.

**(corpus)** the identity also only holds *after* duplicate removal: re-emitted records rewrite
`origin`, so a splitter running before removal over-counts human triggers by up to 15%.

**`origin` is not in one place.** It sits on the record and also on the attachment inside it —
missing the second location loses 27.6% of all trigger markers. And the value is an open set: a third
kind exists beyond the two obvious ones, so anything switching on exactly two values will meet
something it does not expect.

A background agent finishing and the parent resuming is mechanically a new prompt cycle, but nobody
said anything — it is the same interaction continuing. This is exactly the
`agent.call -> launch_ack -> child stream -> agent.output -> runtime.notification -> parent resumes`
chain of §1.5, and it lives INSIDE one Talk.

**Prompt cycle ids are not unique across streams.** A child stream is written under a cycle id that
also appears in the parent, and one id was observed in three different child streams. Keying a Talk
on the cycle alone therefore puts a child's records inside the parent's turn and leaves the child
with no run of its own. The key is (stream, cycle).

*Verification on a real 59,495-record session:* the parent lineage assembles to **307 Talks**, which
is exactly the number of `origin.kind=="human"` records measured independently on the raw file, from
336 distinct prompt cycles.

**Membership is resolved by walking backward, never by line proximity.**

`promptId` appears on `user` records only - assistant records carry none, verified across 115,184.
But every assistant record reaches one through `parentUuid`:

```text
assistant records tested          400
reached a promptId via parentUuid 400      (100%)
hops needed                       min 1, median 2, max 7
```

**(corpus)** the walk must not be capped at a small constant: 140 of 44,973 records need more than 7
hops. Attachments are ordinary hops in the chain — they carry a parent and are named as a parent by
later records — so the walk must pass through them rather than skip anything that is not a message.

So a record's Talk is found by walking back to the nearest ancestor carrying a cycle id, then reading
that record's trigger. (The index exposes these as `Cycle` and `Trigger`; the runtime field names
stop at the adapter.) Line-order proximity would agree in the simple case and diverge exactly where
it matters — a forked chain, a re-emitted block, an interrupt that reattaches to an earlier record —
putting records in the wrong turn.

**A child stream is exactly one Talk**: the delegated prompt in, the final output out, nobody outside
the agent involved. Child streams never reset context, so each is also exactly one epoch.

This one is asserted from the stream's nature, not measured from the data. **(corpus)** child
records carry `promptId` on 82,378 of 221,592 records but `origin` on exactly **one** record in the
whole child corpus — so a child stream has many cycles and essentially no triggers, and the rule
cannot be derived the way it is on the parent lineage. The implementation creates the single Talk up
front and files every cycle in it as a Run.

**Human turns that exist only as attachments must be recovered.** These are `queued_command` records
with no message twin, and a reader following messages alone loses real human input — **(corpus)** a
third of all human utterances live only here. They are filtered on `commandMode == "prompt"`; without
the mode, completion blobs queued the same way are read as things the user typed.

**They are not Talk starts.** **(corpus)** 886 of 918 are mid-turn steering absorbed into a cycle
already running. They belong *inside* the enclosing Talk as human content, placed by ancestry — and
no cycle id may be invented for them.

**Ownership rules** from the model: `context.injection` is first-class, `message.synthetic` is never
counted as agent output, `agent.launch_ack` is not a result, and `agent.output` is owned by the
child stream and only referenced by the parent.

### 1.8 Close segments — candidate, then gates

A quiet period creates a **close candidate**, not a commit, because a long gap often falls inside a
turn rather than between two.

**(corpus)** the "~32%" of the first draft was one number answering two different questions. Both
matter, and the gate depends on the second:

| Gap threshold | Candidates | Mis-splits a prompt cycle | Mis-splits a **Talk** |
| --- | --- | --- | --- |
| 10 min | 697 | 23.67% | **38.74%** |
| 15 min | 444 | — | 30.41% |
| 20 min | 302 | 9.60% | 21.52% |

The Talk figure is the one to gate against: splitting at a notification cycle still severs a Talk.
There is nothing special about ten minutes — it is the worst point on the curve below fifteen. The
threshold is a setting, recorded in the round header's policy version, and 20 minutes buys a large
reduction in mis-splits for 57% fewer candidates. Whether provider-error records sit on the
segmentation timeline moves the first column by five points, so that choice is stated too.

**Time runs backwards sometimes.** **(corpus)** 0.315% of consecutive records step back, by up to
4.06 hours, and provider-error records are the main cause: their timestamp is when the failed
request began, not when the record was written. Anything ordering by time — the gap computation
included — must clamp a negative delta rather than read it as an enormous gap.

Gates before committing:

| Gate | Why |
| --- | --- |
| `activity_boundary` | the idle gap is real |
| `no_crossing_open_operation` | **no tool request whose result lands on the far side of the gap, and no child still running.** Without this a request could be frozen into a committed segment while its result arrives in the next one, and a committed segment is never rewritten |
| `lateness_watermark` | no late evidence still expected |
| `conversation_identity` | the conversation id is resolved |

The second gate does real work rather than being a safety net: it is what makes "committed segments
are immutable" safe to promise.

**(corpus)** it is not about a request left dangling at the end of a file — that is 0 of 25,892 on
the parent lineage, because the runtime always writes a result, including a failure. It is about a
result arriving after the quiet period, which happens on 85 of 697 long gaps (12.2%). The two are
different things and the first draft conflated them.

---

## 2. Output structure — the round chain

Implemented in [`pkg/sessionflow`](../pkg/sessionflow/). Its tests are the specification;
what follows is why it has this shape.

**A round is an append-only immutable delta, and the conversation is the fold of every round.**

```text
state₀ = empty
stateₙ = apply(stateₙ₋₁, roundₙ)
```

The obvious shape — rewrite the whole structure every round — works against everything beneath
it. Landed files append, cursors advance, the index appends. Rewriting a 15 MB structure every five
seconds to add a handful of nodes is the one place the design would stop being incremental.

Pure append does not work either, because **assembly revises what it already emitted**. A `tool`
node lands with `result: unavailable` and gains its result later. A provider call gains fragments.
An `agent.call` gains its child stream once the child transcript appears — and the workflow journal
names children before their transcripts exist, so this is routine, not an edge.

The resolution is that a revision is a *new line in a later round*, never an edit to an earlier one.
That is what lets a round be digested, archived or shipped the instant it is written.

### 2.1 Layout

```text
data/_conversations/<conversation-id>/
  conversation.state                       mutable head pointer
  rounds/r000001-<digest12>.sf            immutable, 0444
  rounds/r000002-<digest12>.sf
```

The split is deliberate. **Rounds carry nothing mutable and nothing temporal**; `conversation.state`
carries the head round, its digest, how far landed evidence has been consumed, and when. Rounds are
linked by digest, not by filename: round N names the digest of round N−1, so a holder of round 3
that has not seen round 2 can store it but cannot apply it.

### 2.2 Frames

```jsonl
{"t":"header","schema":"sf/1","conversation":"C1","session":"S1","round":2,"previous":"<r1 digest>",
 "from_seq":46,"through_seq":57,"input_digest":"<chained landed digest>","parser":"v1","policy":"v1"}
{"t":"node","id":"main/t8","revision":2,"kind":"talk","stream":"main"}
{"t":"node","id":"main/t8/r1/c3","revision":2,"kind":"llm.call","parent":"main/t8/r1",
 "ref":{"seq":12,"row":340},"refs":[{"seq":12,"row":340},{"seq":12,"row":341}]}
{"t":"relation","id":"rel/starts/main-t8-r1-c3/stream-a4646a5c9","revision":2,"type":"starts",
 "from":"main/t8/r1/c3","to":"stream/a4646a5c9","quality":"exact_unique","via":"toolUseResult.agentId",
 "evidence":[{"seq":12,"row":345}]}
{"t":"unresolved","id":"unres/tool_result/toolu_01Y","revision":2,"kind":"tool_result",
 "ref":"tool/toolu_01Y","state":"open"}
{"t":"commit","digest":"<sha256 of lines 1..n-1>","counts":{"nodes":1284,"relations":96,"unresolved":3}}
```

The commit digest covers every line before it, so a round verifies itself: recompute over lines
1..n−1 and compare. It is also the value the next round names as its `previous`.

### 2.3 The four rules

**1. Entity ids come from stable evidence, never from position.** A node keyed by "the 14th record in
the stream" gets a different key the moment an earlier source is backfilled — and a changed key
cannot supersede its own earlier revision, so the fold would hold both, one stale and one current,
with no way to tell them apart. Ids are built from the runtime's own identity for a thing, scoped by
session and stream; where the runtime supplies no identity, from the landed location, which is
itself immutable once written.

**2. Last writer wins, per id, whole-entity.** A later revision replaces the earlier one entirely.
Not field-by-field: a partial merge cannot express "this attribute is now absent", so a correction
that removes something could never be recorded.

**3. Absence means unchanged.** An entity missing from round 7 is exactly the entity round 4
published. Removal is therefore explicit — a revision with `tombstone` set — and never inferred from
silence. Likewise an unresolved entry that gets resolved is *superseded* with `state:"resolved"`, not
deleted: absence would be indistinguishable from "never noticed".

**4. Order is chain order, not timestamp order.** Rounds fold in round order, entities within a round
in file order. Timestamps come from the observed runtime and can go backwards across sources; the
chain cannot.

### 2.4 Determinism, and what it forced

The property worth protecting is: *same previous digest + same landed range + same parser and policy
versions ⇒ same round bytes and same digest*. Without it a chain can only be trusted, never
re-derived. Three things in the earlier draft broke it, and each had to change:

- **`revision` was a per-entity counter.** Deriving it required replaying the chain, and two
  producers reaching the same state by different paths would disagree. It is now **the round number
  that produced the entity** — readable straight off the header, and identical for any producer of
  that round.
- **The header carried a timestamp.** Wall-clock time cannot be reproduced, so it would put every
  re-derivation in disagreement with the original. Time moved to `conversation.state`, outside every
  digest.
- **`input_digest` was described as a digest over all evidence read so far.** Recomputing it would
  make each round cost proportional to the whole conversation — a long-running one would slow down
  without bound, which is the exact property an incremental design exists to avoid. It is now
  chained: `H(previousₙ₋₁ ‖ sorted digests of what round n consumed)`, proportional to what is new
  and still binding every round transitively to all evidence before it. Inputs are sorted because
  directory iteration order is not stable.

### 2.5 What verification checks

`Chain.Verify` walks the chain and checks four things, each catching a failure the others miss:

| Check | Catches |
| --- | --- |
| rounds are 1..N with no gap | a missing round — its successors are unverifiable without it |
| each round names its predecessor's digest | a fork, or an edited round |
| each round's digest matches its bytes | tampering or truncation (`Read` does this) |
| `from_seqₙ == through_seqₙ₋₁ + 1` | **skipped landed evidence — no digest reveals this** |

The last is the one worth stating separately. Digests prove the rounds are the rounds; only the
sequence check proves they read everything.

### 2.6 What shipping does not solve

A round references content by `{"seq":…,"row":…}` into landed files. A receiver holding only rounds
can render the structure but not the text. Either the landed files travel too, or the export inlines
content for the nodes it describes — an export contract decision, deferred to Plan 03.

**Content is referenced, never inlined**, for the same reason: storing each call's input would
multiply storage by conversation depth — roughly 220 MB for a session whose actual content is 10 MB.

**Input manifests are deltas.** `base` + `appended`, which is exactly the continuity invariant, so
the representation *is* the claim. A full manifest appears only where the chain genuinely breaks:
compaction, fork, stream start. Those bases coincide with epoch boundaries.

**Containment is single-parent; causality is a sparse typed relation** carrying `via` and `quality`,
so an exact id match is distinguishable from an inference.

---

## 3. Continuous build

Rounds and segments are independent axes: the round is the **append** unit, the segment is the
**commit** unit. Many rounds accumulate while work continues; a segment closes only on an idle gap
plus gates. A segment close is recorded as an ordinary round — one that publishes the segment
boundary — not as a separate mechanism or a rewrite.

Watermark chain, each stage trailing the last:

```text
assembled_seq  <=  indexed_seq  <=  next_seq - 1
```

where `assembled_seq == conversation.state.through_seq`. A crash between publishing a round and
saving state leaves a round on disk that state does not mention, so **the filesystem is the
authority**: recovery lists the rounds directory and takes the highest, rather than trusting the
pointer.

**A per-stream record-id index must persist across commits.** A resume replays records whose first
occurrence may sit in an already-committed segment; without it, a replayed block lands as a
duplicate node. (This is the same rule the derived index enforces in `internal/index`: first
occurrence wins, and the duplicate is excluded from *every* lookup, not only from id resolution.)

**The invariant that keeps this honest:**

```text
fold(rounds 1..N)  ==  full parse of landed evidence 1..through_seqₙ
```

A full rebuild from the index is the reference implementation — simple and obviously correct — and
the fold is checked against it. Where they disagree the fold is wrong, and the disagreement is a
test failure.

The earlier draft asserted something weaker and wrong: that revision N+1 is a *prefix extension* of
revision N. It is not, and cannot be. Late evidence supersedes entities emitted rounds ago, which is
the whole point; a replay reordering after duplicates are removed or a compaction re-rooting an epoch changes what an
earlier round said. Prefix extension holds for the round *chain* — no round is ever rewritten — but
never for the folded result.

**Parser and policy versions are chain generations.** A change to either that alters meaning does not
retroactively fix earlier rounds and must not silently mix with them: it starts a new chain. Both
versions are in every header so a reader can detect the mix rather than fold through it.

---

## 4. The unresolved catalog

Recorded as data, with two disciplines that make it useful rather than noisy.

**Transient vs permanent**, because both genuinely occur:

| Reference | Kind |
| --- | --- |
| journal `started` names an agent whose transcript does not exist yet | transient **while the run is live**; once the run is over it is a pruned or lost file, so it is terminal |
| `<task-notification>` `<tool-use-id>` absent from the file it landed in | transient — resolves elsewhere in the session |
| `tool_use` with no `tool_result` | transient unless the stream ended. **(corpus)** 0 of 25,892 on the parent lineage and 7 of 79,418 on child streams, always the final line of a truncated file |
| workflow child `started` with no `result` | **not an error** — **(corpus)** 10.24% of workflow children never get one |
| `allUuids` entry existing nowhere on disk | terminal — held in context, never written |
| `logicalParentUuid` unresolvable | terminal |
| child stream nothing names as spawned | terminal — **(corpus)** 2 exist |
| source in `source_gone` | terminal — pruned before collection |

**A denominator.** "3 unresolved" means nothing; "3 of 8,045 tool_use ids" means something.

A transient reference is promoted only on **terminal evidence** — a pruned source, a finished run, a
runtime that wrote no such record. Never on elapsed rounds: "still open after N rounds" is a
statement about how often we looked, not about the data. That is why `state` has three values
(`open`, `resolved`, `terminal`) rather than a counter.

---

## 5. Open questions

1. **Does a context reset force a segment close?** I lean no — an epoch is a model-context lifetime,
   a segment is a commit window; treating them as one makes segments depend on the runtime's reset
   timing. There is a reason to revisit it: a reset is preceded by a block of re-emitted records, so
   the two events are already entangled at the record level.
2. **Where on the threshold curve to sit.** The table in §1.8 is measured; the choice is not made.
   10 minutes mis-splits 38.74% of Talks and 20 minutes mis-splits 21.52% at 57% fewer candidates.
   The default is 10 minutes because the gates catch what the threshold gets wrong, but that reasoning
   deserves a measurement of its own — how often the gates actually save a bad candidate.
3. **Where a session's conversation id comes from** when an application supplies one, and how it is
   recorded so cold reactivation preserves it. Today `asz parse` uses the session id, which is the
   adapter's documented default and the only sound one.
4. **How a viewer folds the chain.** The fold this document describes is the assembler's: apply every
   round in order and keep the result. A viewer needs the same answer but not necessarily the same
   path — it may want the state as of an earlier round, or the history of one entity across rounds,
   or to stream rounds as they arrive rather than read them all first. That is Plan 03 work and is
   deliberately not designed here; what Plan 02 guarantees is only that the rounds contain enough to
   support any of those.

5. **Retention now spans two directories.** Landed files under `data/<session>/` and rounds under
   `data/_conversations/<id>/` age independently, and a round references landed records by
   `(seq, row)`. Pruning landed data while keeping rounds leaves a structure whose content cannot be
   read; pruning rounds while keeping landed data is safe but discards the parse. There is no policy
   for this yet, and the coupling should be made explicit before either directory gets a TTL.
