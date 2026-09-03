# Glossary

Every word this project uses with a fixed meaning, and what each one is **not**. It is
runtime-agnostic throughout: no agent product's vocabulary appears here. For what one runtime calls
these things, see the [Claude Code glossary](../adapters/claude-code-glossary.md).

Where a word is easy to confuse with a neighbour, the entry says so — those pairs are where most
misreadings start.

---

## The two boundaries

Everything else is defined relative to these.

**Conversation** — the durable ownership boundary, and the unit of storage, analysis and export.
Its identity is **supplied, never inferred**: an adapter reports it or an application provides it.
Grouping by person, account, timestamp proximity or session id is not permitted.
*Not:* a person, an account, a session, a trace, or a time range.

**ExecutionStream** (usually just **stream**) — the ordered continuity boundary. Model-message
continuity is evaluated inside one and never across two. A child agent always creates or continues a
distinct stream.
*Not:* timestamp order, and not a sequence of prompts.

## Structure

**Segment** — an activity window that can be committed on its own. It sits above Session and cuts
**across** every stream beneath it, so it is a time window rather than a branch of the tree.
*Not:* a turn, a Talk, or a new conversation.

**Session** — the source-runtime context a record came from, carried as provenance. One conversation
may contain several; a session belongs to exactly one conversation.
*Not:* an agent, a stream, or a conversation. It is **not** a grouping key.

**Context epoch** (usually **epoch**) — the lifetime of one model context within a stream. A stream
that never resets has exactly one. A reset is taken only from an explicit boundary record.
*Not:* a token-count threshold, and never inferred from a message-list mismatch.

**Talk** — one readable interaction: an input from outside the agent, the work that followed, and
the output. A Talk starts on external input, or on a run whose records state no trigger at all.
Everything a Talk delegated stays inside it.
*Not:* an exact provider request, and not one per prompt.

**Run** — one loop the agent performed in response to one trigger. A Run averages thirteen provider
calls, so it is emphatically **not** a single model call. The agent does not start its own: every
Run is triggered from outside the loop, by input or by the runtime reporting a child has finished.

**Step** — one observed occurrence inside a Run. The leaves of the tree.
*Not:* an inferred event. A step that cannot be observed is reported unavailable.

## Step kinds

| kind | is |
| --- | --- |
| `message.external` | input originating outside the agent, typically a person |
| `message.assistant` | agent output visible to the requester |
| `message.synthetic` | an assistant-role record **the client fabricated** — a connection loss, a provider error — surfaced inline so the requester sees it. Never counted as agent output. |
| `context.injection` | material the harness put into model context: instructions, memory, catalogues, reminders |
| `llm.call` | one provider attempt: its ordered input and its response |
| `thinking` | reasoning content attached to a response |
| `tool` | one tool use — the request, what ran, and what came back, as **one** step |
| `agent.call` | a request to start or continue a child agent |
| `agent.launch_ack` | acknowledgement that a child started. **Not** its result. |
| `agent.output` | the child's final output, owned by the **child** stream |
| `runtime.notification` | the runtime telling the parent a child completed |
| `epoch.boundary` | an explicit model-context reset |
| `epoch.summary` | the carried-forward summary that reset produced |
| `error.api` | a provider error |
| `control.interrupt` | an externally initiated interruption |
| `control.permission` | a permission decision or mode change |
| `control.command` | a runtime command invoked in-line |
| `turn.duration` | a runtime-reported duration for a completed turn |

**A tool use is one step, not three.** The request and the result are separate records, often far
apart, but they pair one to one and a reader thinks of them as a single event. A tool whose result
never arrived carries `result: unavailable` — it is not dropped and not given an empty result.

## Relations

The tree carries **containment only**: every node has at most one parent. Everything else is a
typed relation carrying its own correlation quality. Cross-stream flow is never containment.

| relation | reads as |
| --- | --- |
| `starts` | an agent call → the child stream it created |
| `reports` | a notification → the child that finished |
| `ends_with` | a child stream → the step carrying its final output |
| `result_of` | a record → the tool use whose result it carried |
| `follows` | an epoch → the epoch it continues after a reset |
| `summarizes` | a summary → the reset that produced it |
| `in_segment` | a Talk → the window that will commit it |
| `retries` | an attempt → the attempt it replaced |
| `cancels` | an interruption → the operation it ended |
| `input_of` | a record → the provider call whose input it was part of |

Each name is a verb, so an edge reads as a sentence from its start to its end.

## What a record carries

The role names a record's identifiers play. A runtime's own field names stop at its adapter; these
are what everything above it sees.

| role | is |
| --- | --- |
| `id` | this record's own identity |
| `parent` | its containment parent |
| `call` | the provider call it is a fragment of |
| `run` | the agent loop it belongs to |
| `batch` | the group of children it was started with. A join key; nothing in the model corresponds to it. |
| `stream` | the lineage it was written in |
| `continues` | the record a new model context resumes from, across a reset |
| `tool` | the tool use this record is about, where the runtime says so outside the content |
| `child` | the child agent stream this record names |
| `time` | when the runtime says it happened |
| `trigger` | what started the run: `external`, `notification`, or `unknown` |

## Qualification

Nothing is presented as observed unless it was observed.

**Correlation quality** — on every join between two records:

`exact_unique` · `exact_ambiguous` · `strong_inference` · `weak_inference` · `unresolved` ·
`conflict`

An exact identifier does **not** guarantee a unique match. Where several candidates share a key the
relation stays `exact_ambiguous` and nothing chooses one. A `conflict` is never overwritten by a
later agreeing observation.

**Claim state** — how a claim was arrived at: `observed_replayable` · `observed_report_only` ·
`proposed` · `unavailable`.

**Content state** — for any referenced payload: `available` · `redacted` · `omitted` · `hash_only` ·
`size_only` · `truncated` · `unavailable`.

## Storage

**Landed record** — a source record copied into the storage zone, byte-for-byte. Write-once and
read-only: temp file, fsync, rename, `chmod 0444`. Never appended to.

**Delta** — one file of landed records, covering a contiguous run of a source. A source becomes many
deltas over time.

**Cursor** — how far collection got in one source, committed **after** the data lands. That order
makes a crash repeat data rather than lose it.

**Sequence** — a landed file's number, monotonic across every stream in a session. It is what lets
progress be tracked with a single watermark.

**Watermark** — how far a stage has got, as a sequence. They trail each other:
`assembled ≤ indexed ≤ landed`.

**Index** — a derived lookup structure over landed records. Holds identifiers, never content.
**Disposable**: delete it and it rebuilds; a schema change discards rather than migrates. Nothing
downstream may treat it as authoritative.

**Round** — one immutable, digest-chained delta of conversation structure. Round N names round N−1's
digest. A round carries no wall-clock time, so the same evidence reproduces the same bytes.

**Chain** — a conversation's rounds in order.

**Fold** — applying every round in order. `fold(rounds 1..N)` **is** the conversation as of round N;
there is no other state. Last writer wins per entity; absence means unchanged; removal is an
explicit tombstone; order is chain order and never timestamp order.

**Revision** — the round number that produced a version of an entity. Derived from chain position,
not counted per entity, so a round can be re-derived without replaying the chain.

## Adapters

**Adapter** — what turns one runtime's data into landed records. Named for the runtime **and** the
collection posture, because the posture changes what can be promised about completeness.

**Transport** — how records arrive: reading local files, or receiving a push.

**Dialect** — how records are interpreted: one runtime's schema. A push receiver and a local reader
for the same runtime share a dialect and nothing else.

**Glossary** — what one dialect calls each thing named here, so a reader can see either vocabulary.
An empty entry means the runtime has no word for it, which is an answer rather than a gap.
