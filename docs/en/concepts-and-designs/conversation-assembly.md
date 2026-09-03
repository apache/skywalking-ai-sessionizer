# Conversation Assembly

Assembly turns landed records into the structure defined by the
[Unified Conversation Model](unified-conversation-model.md), and appends the result to an
append-only chain of immutable rounds.

It reads the **derived index**, not the payloads. Structure resolution needs identifiers, not
content: removing duplicate records needs record ids, grouping a provider call needs message ids,
joining a tool needs tool-use ids, joining a spawn needs agent and run ids. Message text is read only
when a conversation is rendered, and only for the records that appear in it.

```sh
asz parse                     # assemble every session, append a round to each chain
asz parse SESSION             # one session
asz conversation ID           # fold the chain and show the structure
asz verify                    # check landed data and every round chain
```

## The pipeline

Eight stages. The order is forced: each depends on the one before it.

| Stage | What it establishes |
| --- | --- |
| 1. Remove duplicate records | nothing downstream is correct first; the first copy wins |
| 2. Partition streams | one ordered lineage per agent, from the file it was written in |
| 3. Group provider calls | by message id, in line order |
| 4. Join tools | request and result become one step |
| 5. Join child agents | which call started which child stream |
| 6. Cut context epochs | only from an explicit reset record |
| 7. Build Talks and Runs | the conversation a person reads |
| 8. Propose segments | activity windows that can be committed |

Six rules in here are the opposite of the obvious choice, and each is the opposite because a
measurement said so.

**A record type is not what a record is.** Every tool result in the corpus sits on a record whose
type is `user`. Only a small fraction of `user` records are things a person said. Classification
reads the trigger and the content blocks, never the type alone.

**Keep the first copy of a repeated record, not the last.** A runtime re-writes a block of history
just before it resets model context. The later copy is the worse one: it has a rewritten prompt
cycle, and in many cases its captured tool output has been blanked. Keeping the last both moves
records away from their true position and destroys data.

**Group a provider call by message id, in line order.** Never by walking the parent pointer forward,
which drops a tool use on nearly one call in five; and never by request id, because a record the
client fabricated can carry a real call's request id.

**Take usage from the last fragment in line order.** Not from the fragment with a stop reason — a
parent lineage stamps the same stop reason on every fragment — and never by adding fragments up,
which multiplies the real number by the fragment count.

**A Talk starts on input from outside the agent, or on a cycle with no stated trigger at all.** A
background agent finishing and the parent resuming is mechanically a new prompt cycle, but nobody
said anything; it is the same interaction continuing. A locally typed command is the reverse case —
a person acting with no trigger recorded — so both are Talk starts and everything between them is
not.

**Membership is resolved by walking backward.** A model response carries no cycle id but reaches one
through its containment parents, which is single-valued. Line proximity agrees in the simple case and
disagrees exactly where it matters.

## What comes out

The containment tree carries direct ownership only. Every node has at most one parent.

```text
session
 └ stream
    └ epoch
       └ talk
          └ run
             ├ llm.call
             │  ├ message.assistant · thinking
             │  └ tool
             ├ message.external · context.injection
             ├ agent.call → agent.launch_ack
             └ runtime.notification
```

Everything else is a **typed relation** carrying its own correlation quality: `starts`, `reports`,
`ends_with`, `result_of`, `follows`, `summarizes`, `in_segment`. Cross-stream flow is never containment,
which is what stops a rendered conversation repeating every subagent's work inside its parent.

A Segment is a relation rather than a parent. It is a time window and a session outlives many of
them, so a session cannot sit under a segment in a tree where every node has one parent.

## The round chain

A round is an immutable delta. The conversation is the fold of every round from the first to the
latest.

```text
data/_conversations/<conversation-id>/
  conversation.state                       the mutable head pointer
  rounds/r000001-<digest>.sf              immutable, read-only
  rounds/r000002-<digest>.sf
```

Later evidence — a tool result that arrives after its call, a child transcript that appears after its
spawn — produces a new revision in a **later** round, never an edit to an earlier one. That is what
lets a round be digested, archived or shipped the moment it is written.

Three rules govern the fold, and each exists because its opposite loses information:

1. **Last writer wins, per id, whole entity.** Not field by field: a partial merge cannot express
   "this is now absent", so a correction that removes something could never be recorded.
2. **Absence means unchanged.** An entity missing from round 7 is exactly what round 4 published.
   Removal is explicit, and an unresolved reference that gets resolved is superseded with that state
   rather than deleted — absence cannot say that a gap existed and closed.
3. **Order is chain order, not timestamp order.** Timestamps come from the runtime and can run
   backwards; the chain cannot.

Rounds are linked by digest, not by filename: round N names the digest of round N−1. A round carries
no wall-clock time, so the same landed evidence and the same parser version reproduce the same bytes
and the same digest. Anything mutable or temporal lives in `conversation.state`, outside every
digest.

A viewer folds the same chain, but not necessarily the same way: it may want the state as of an
earlier round, or one entity's history across rounds, or to render rounds as they arrive. That is a
later concern. What the chain guarantees is that the rounds hold enough to answer any of them.

`asz verify` checks four things. Three are digest properties — no gap in the round numbers, each
round names its predecessor's digest, each round's digest matches its bytes. The fourth is that
consumed landed sequences are contiguous across rounds, which catches skipped evidence that no digest
would reveal.

## Qualification

Nothing is presented as observed execution unless it was observed.

Every join carries a correlation quality: `exact_unique`, `exact_ambiguous`, `strong_inference`,
`weak_inference`, `unresolved`, `conflict`. An exact identifier does not guarantee a unique match —
where several candidates share a key the relation stays `exact_ambiguous` and the assembler does not
choose one.

What could not be resolved is carried as data, with a denominator. An assembler that drops what it
could not resolve presents a partial conversation as a complete one. An entry is promoted to terminal
only on terminal evidence — a pruned source, a finished run — never because it has stayed open for a
number of rounds, which is a statement about how often we looked rather than about the data.
