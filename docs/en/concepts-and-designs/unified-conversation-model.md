# Unified Conversation Model

SkyWalking AI Sessionizer assembles fragmented agent telemetry into one durable conversation structure. This
document defines that structure. It is **runtime-agnostic**: every concept here must be expressible
by any adapter, and no concept assumes a particular agent product. Runtime-specific mappings live
under [Adapters](../adapters/claude-code.md).

## Why a model is needed

Agent runtimes expose traces, logs, transcripts, provider request bodies, tool events and subagent
metadata as separate records. A single user-visible conversation can outlive one process, reactivate
after a long idle period, and contain several concurrent agent execution lineages.

Trace-level inspection alone cannot answer: what did the whole conversation do; which input, model
response, tool result or child-agent result led to the next model call; did that call preserve the
previous message history or start a new context; which agents contributed; and what should be
measured, stored and exported as one unit.

The model answers those by fixing two boundaries:

- **Conversation** is the durable aggregation and ownership boundary.
- **ExecutionStream** is the ordered continuity boundary.

Everything else is defined relative to those two.

## Hierarchy

```text
Conversation                          durable identity · ownership boundary
 └ Segment                            activity window · the COMMIT unit
    └ Session                         observed source provenance
       ├ ExecutionStream  main        ordered parent-agent lineage
       │  └ Context epoch × N         model-context lifetime
       │     └ Talk × N               one interaction from outside → run → output
       │        ├ Input
       │        └ Run                 the agent loop
       │           └ Step × N         the leaves — see "Step layer"
       └ ExecutionStream  child × N   independent context per child agent
          └ Talk (typically 1)
             └ Run → Step × N
```

`Segment` sits above `Session` and **cuts across every stream beneath it**: it is a time window
chosen for commit, not a branch of the stream tree. A segment is committed only once all of its
streams are safe to freeze.

`Context epoch` exists **only where a runtime can reset model context in place**. A stream with no
compaction always has exactly one epoch, and its message history accumulates without interruption.

A `Talk` **begins** where input arrives from outside the agent. It is not one input. A runtime that
accepts a message while the agent is still working delivers it into the cycle already running, and
that message belongs to the interaction it interrupted, not to a new one. So a Talk carries every
input that arrived before it ended.

Measured over 361 talks drawn from 43 real conversations: 75.3% carry one input, 15.8% carry two,
5.3% carry three, and 2.5% carry four or more. Only 24% carry exactly one input and one reply. A
reader that assumes one input per Talk is wrong about a quarter of them.

The same holds on the way out. The agent speaks between tool calls, so a Talk usually holds several
assistant messages and only the last is the reply. Anything that renders a Talk as a single exchange
has to say which of them it means.

### Object definitions

| Object | Is | Is **not** |
| --- | --- | --- |
| **Conversation** | stable semantic branch; the unit of storage, analysis and export | a person, an account, a session, a trace, or a time range |
| **Segment** | independently committable activity window | a turn, a Talk, or a new conversation |
| **Session** | source-runtime context carried as provenance | an agent, a stream, or a conversation |
| **ExecutionStream** | ordered main, child, auxiliary or judge lineage within one session | timestamp order, or a prompt sequence |
| **Context epoch** | the lifetime of one model context within a stream | a token-count threshold |
| **Talk** | one interaction that began outside the agent | one input, or an exact provider request |
| **Run** | one agent loop inside a Talk | a single model call |
| **Step** | one observed occurrence inside a run | an inferred event |

### Identity rules

- **Conversation identity is supplied, never inferred.** An adapter reports it, or an application
  supplies it. Grouping by person, account, timestamp proximity or session id is not permitted.
- **A session is provenance, not a grouping key.** One conversation may contain several sessions; a
  session belongs to exactly one conversation.
- **A child agent always creates or continues a distinct execution stream.** It creates a new session
  only when the source runtime explicitly reports a different session identifier. Visual nesting,
  agent identity and timestamps never manufacture a session.
- **A conversation id survives cold reactivation** while its durable identity mapping exists. A TTL
  purge starts a new generation.

## Step layer

Steps are the leaves of the tree — the observed occurrences inside a run. Every step carries its own
provenance and qualification (see below); a step that cannot be observed is reported as unavailable
rather than synthesised.

| Family | Kind | Meaning |
| --- | --- | --- |
| **Message** | `message.external` | input originating outside the agent (typically human) |
| | `message.assistant` | agent output visible to the requester |
| | `message.synthetic` | an assistant-role record **fabricated by the client**, not returned by the provider — connection loss, provider error, quota or auth failure, surfaced inline so the requester sees it |
| **Context** | `context.injection` | material the harness injected into model context — instructions, memory, catalogues, reminders, environment state |
| **Model** | `llm.call` | one provider attempt: ordered input manifest plus response |
| | `thinking` | reasoning content associated with a model response |
| **Tool** | `tool` | one tool use: the request, what ran, and what came back — see below |
| **Agent** | `agent.call` | a request to start or continue a child agent |
| | `agent.launch_ack` | acknowledgement that a child started — **not** its result |
| | `agent.output` | the child's final output, owned by the **child** stream |
| | `runtime.notification` | the runtime informing the parent that a child completed |
| **Epoch** | `epoch.boundary` | an explicit model-context reset |
| | `epoch.summary` | the carried-forward summary produced by that reset |
| **Control** | `error.api` | a provider error, with retry state where available |
| | `control.interrupt` | an externally initiated interruption |
| | `control.permission` | a permission decision or mode change |
| | `control.command` | a runtime command invoked in-line |
| | `turn.duration` | a runtime-reported duration for a completed turn |

**A tool use is one step, not three.** It carries `name`, `input` (for a shell tool, the command
itself), and `result`, each referencing the record it came from:

```text
tool
 ├ name    "Bash"
 ├ input   { command: "…", description: "…" }   ← the request
 ├ result  { output, is_error, … }              ← what came back
 └ timing  duration, where the runtime reports it
```

Request and result are separate records in most runtimes, often far apart in the stream, but they
pair one-to-one and a reader thinks of them as a single event: *it ran this, and got that back*.
Splitting them into sibling steps triples the node count and forces every consumer to rejoin what
the model already knows belongs together.

Two states must stay expressible rather than being hidden. A tool use whose result never
arrived - interrupted, or the session ended - carries `result: unavailable`; it is not dropped, and
it is not given an empty result. And where a runtime does not report execution timing separately
from record timestamps, `timing` is `unavailable` rather than inferred from the gap between records.

Four ownership rules govern this layer:

1. **`message.synthetic` must never be counted as agent output.** It carries the assistant role and
   sits in the ordered stream like any response, but no model produced it. Counted as
   `message.assistant` it corrupts output statistics and attributes text to the agent that the agent
   never wrote. It is also positive signal — it marks precisely where a conversation was interrupted
   — so it is separated rather than discarded.
2. **`context.injection` is first-class, not decoration.** Injected material shapes a response as much
   as the human turn does, and in some runtimes it is the *only* record of an input.
3. **`agent.output` belongs to the child stream.** The parent receives a compact boundary that
   *references* the child output; it never absorbs the child's messages or tools. This is what
   prevents duplication when a conversation is rendered or exported.
4. **`agent.launch_ack` is not a result.** For asynchronous delegation the canonical order is
   `agent.call → agent.launch_ack → child stream → agent.output → runtime.notification → parent's
   next llm.call`.

## Model-context continuity

Within one stream and one context epoch, adjacent eligible provider calls are expected to satisfy:

```text
next_request.messages =
      previous_request.messages
    + assistant(previous_response)
    + new_input_messages
```

Four properties of this check matter:

- Comparison is **recursive semantic equality, not byte equality**. Cache annotations migrate between
  requests and must be excluded from the comparison, or the invariant is false on every adjacent pair.
- It applies **within one stream and one epoch only**. Auxiliary, retry and reset transitions are
  excluded, and it never crosses streams.
- An explicit epoch boundary is the **only** admissible evidence of a context reset. It is never
  inferred from token counters or from a prefix mismatch alone.
- The strength of the check depends entirely on the evidence available. Where a runtime exposes the
  serialized request, this verifies what was actually sent. Where it does not, the same equation
  evaluated over a reconstructed message list is a **structural cross-check on the reconstruction**,
  not a statement about the wire. Adapters must report which of the two applies.

## Evidence and qualification

Every material claim, statistic, diagram edge and rendered annotation carries or inherits a
qualification. Nothing is presented as observed execution unless it was observed.

**Claim states**

| State | Meaning |
| --- | --- |
| `observed_replayable` | derived from retained evidence that can be replayed |
| `observed_report_only` | observed, but the underlying evidence is not retained |
| `proposed` | a design target, not yet produced by an implementation |
| `unavailable` | the source cannot supply it |

**Correlation quality** — every join between two records:

| State | Meaning |
| --- | --- |
| `exact_unique` | an exact identifier match with exactly one candidate |
| `exact_ambiguous` | an exact identifier match with several candidates — **never silently resolved** |
| `strong_inference` | no identifier, but a well-founded structural argument |
| `weak_inference` | plausible only |
| `unresolved` | no usable evidence |
| `conflict` | sources disagree |

An exact identifier does not guarantee a unique match. Where several candidates share a key, the
relation stays `exact_ambiguous` and the assembler must not choose one.

**Content state** — for any referenced payload: `available`, `redacted`, `omitted`, `hash_only`,
`size_only`, `truncated`, `unavailable`.

## Ownership versus causality

The tree carries **direct semantic ownership only**. Every node has at most one containment parent.

Everything else — provider input membership, spawn, asynchronous delivery, retry, join, cancellation
and compaction lineage — is a **sparse typed relation** between nodes, each carrying its own
correlation quality and source references. Cross-stream flow is never expressed as containment.

Display order is a deterministic projection. It does not prove causality, and it must not be used as
evidence for it.

## Adapter contract

An adapter supplies as much of the model as its runtime exposes, and reports the rest as
`unavailable`. It must never invent identity, parentage or causal links.

**Minimum for a useful adapter**

| Requirement | Notes |
| --- | --- |
| Conversation identity | explicit; or a documented, stated default mapping |
| Session identity | as observed from the runtime |
| Execution-stream identity | main and child lineages distinguishable |
| Ordered steps | with a stable per-source ordinal |
| Tool call ↔ result join | with correlation quality |
| Child spawn and return | with correlation quality |

**Strongly recommended**

Context-epoch boundaries; ordered provider input manifests; injected-context records; retry and
attempt identity.

An adapter that cannot expose ordered provider input must report model-context coverage as
`unavailable`. Timing and cache counters do not prove model-input membership.

## Non-goals

- Grouping work by human or principal identity.
- Inventing missing model input, sessions, parentage or causal links.
- Re-exporting the original discrete traces and logs as the final product.
- Acting as a general observability backend or product UI.
- Making an unavailable field look like an observed execution-tree node.
