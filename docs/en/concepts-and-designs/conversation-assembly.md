# Conversation Assembly

Assembly turns landed records into the structure defined by the
[Unified Conversation Model](unified-conversation-model.md), and appends the result to an
append-only chain of immutable rounds.

It reads the **derived index**, not the payloads. Structure resolution needs identifiers, not
content: removing duplicate records needs record ids, grouping a provider call needs message ids,
joining a tool needs tool-use ids, joining a spawn needs agent and run ids. Message text is read only
when a conversation is rendered, and only for the records that appear in it.

**Every parse assembles the whole session from its index**, not only the records that arrived since
the last round. So the same evidence always gives the same entities, and a round can be re-derived
rather than only trusted. [The round chain](#the-round-chain) says how the result becomes a round.

Where this page says *in the corpus*, it means one corpus of Claude Code history: 2,970 source
files holding 365,825 records, 1.09 GB in all, from 25 project directories. The date of that
measurement is unavailable.

Assembling everything each time is cheap because the index is small. In the corpus, 365,554 index
entries describing 1.0 GB of landed payload took 47 MB of index, about 21 times smaller. The largest
single session's index is 16 MB, and it loads in 4.5 ms with every lookup map rebuilt. That is why
the lookup maps are rebuilt on load and never stored. [Storage Root](../formats/storage-root.md#size)
gives the index size on a later sample.

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
| 1. Remove duplicate records | the first copy of each record id wins, and nothing downstream is correct before it |
| 2. Partition streams | one ordered lineage per agent, from the file it was written in |
| 3. Group provider calls | by message id, in line order |
| 4. Join tools | request and result become one step |
| 5. Join child agents | which call started which child stream |
| 6. Cut context epochs | only from an explicit reset record |
| 7. Build Talks and Runs | the conversation a person reads |
| 8. Propose segments | activity windows, and whether each could be committed |

Seven rules in here are the opposite of the obvious choice, and each is the opposite because a
measurement said so.

**A record type is not what a record is.** Every tool result in the corpus sits on a record whose
type is `user`. Only a small fraction of `user` records are things a person said. Classification
reads the trigger and the content blocks, never the type alone.

**Keep the first copy of a repeated record, not the last.** A runtime re-writes a block of history
just before it resets model context. The later copy is the worse one: it has a rewritten prompt
cycle, and in many cases its captured tool output has been blanked. Keeping the last both moves
records away from their true position and destroys data.

The key is the record id across the whole session, not within one file. First means first in
landed order: the lower sequence, then the lower row. So the same rule also removes the copies that
a crash between landing a file and committing its cursor leaves in a later file, as
[Storage Root](../formats/storage-root.md#landed-files) describes. A record with no id is never a
duplicate, because a key built on an empty id would collapse every such record into one. In the
corpus, 44,585 of 365,825 records carry no id.

A repeated copy is left out of every lookup, not only the lookup by id. In call grouping it would
inflate a call's fragment count. In the tool join it would make an exact join look ambiguous. In a
stream it would appear twice. The copy stays in the index as evidence of what the runtime wrote
again.

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

**A run id belongs to its stream.** A run id is not unique across a session. A child stream is
written under a run id that also appears in the parent, and one id has been seen in three different
child streams. Keyed on the id alone, a child's records land inside the parent's Talk, and the child
gets no run of its own. So every Talk and Run is keyed by the stream and the run id together.

**Membership is resolved by walking backward.** A model response carries no cycle id but reaches one
through its containment parents, which is single-valued. Line proximity agrees in the simple case and
disagrees exactly where it matters. The walk passes through records of every kind. An attachment
carries a parent and is named as one, so a walk that skipped every record that is not a message
would break. The walk follows at most 64 parent links, and the limit exists only so that corrupt
data cannot loop. A small limit would be wrong: in the corpus, 140 of 44,973 records need more
than 7.

One rule is asserted rather than measured.

**A child stream is always exactly one Talk.** Every run in it is a Run inside that Talk: the
delegated prompt comes in, the final output goes out, and nobody outside the agent takes part. The
Talk's trigger is `unknown`, because the parent wrote the prompt. The rule follows from what a child
stream is. The main stream's rule cannot stand in for it, because that rule reads the trigger each
run states, and in the corpus exactly one of 221,592 child records states a trigger. Applied to a
child, it would start a new Talk on every run.

### Segments

Stage 8 proposes segments. It never commits one.

Only the main stream is cut. Its Talks are taken in order of their first record time. A new window
opens where the gap between one Talk's last record and the next Talk's first record is longer than
the idle gap. Only records that resolve to a run count toward a Talk's time range. A window's id
comes from where it opens, the landed position of its first Talk, and not from its place in the
list. So late evidence that adds an earlier window cannot renumber a later one. The last window is
`open`, and every earlier one is a `candidate`. A child stream's Talk joins the window whose time
range holds its first record, as `strong_inference`, because delegated work belongs to the window
of the Talk that asked for it.

**A negative gap is no gap.** Record times can step backward. In the corpus, 0.315% of consecutive
records step back, by up to 4.06 hours. Provider-error records are the main cause: their time is
when the failed request began, not when the record was written. So a negative difference between
two Talks is treated as no gap, never as a large one.

**The idle gap is 10 minutes, and a gap only proposes.** A long gap often falls inside a turn rather
than between two, so it cannot decide on its own. In the corpus:

| Idle gap | Gaps that long | Share inside a run | Share inside a Talk |
| --- | --- | --- | --- |
| 10 minutes | 697 | 23.67% | 38.74% |
| 15 minutes | 444 | unavailable | 30.41% |
| 20 minutes | 302 | 9.60% | 21.52% |

The Talk column is the one that matters. A window cut inside a Talk splits one interaction between
two windows. Whether provider-error records count on this timeline moves the run column by five
percentage points. In asz such a record counts only when its containment parents reach a run,
because only those records set a Talk's time range.

Twenty minutes would propose 57% fewer candidates. The default stays at 10 minutes because the gates
below decide what could be committed, and a shorter gap only gives them more candidates to judge.
How often the gates stop a bad candidate has not been measured.

The gap is fixed in the build. No configuration key and no command flag changes it. It is part of
the policy every round header carries, `v1+idle=10m0s`, so windows proposed under one gap never
fold together with windows proposed under another. [Session Flow](../formats/session-flow.md#header)
says how a chain refuses a change of policy.

**Four gates decide whether a window could be committed.**

| Gate | Passes when |
| --- | --- |
| `activity_boundary` | the window is not the last one |
| `no_crossing_open_operation` | every tool use whose request time falls inside the window has exactly one result, also timed inside the window, and the session holds no open `child_stream` or `notified_child` entry. The second test covers the whole session, not the window. |
| `lateness_watermark` | the window is not the last one, and the main stream's last record is more than one idle gap after the window ends |
| `conversation_identity` | the conversation id is not empty. Assembly refuses to run without one, so this gate always passes. |

A segment node carries `state`, `talks`, `gates_unmet` and `committable`. `gates_unmet` lists the
gates that fail, and `committable` is true when none fail. [asz.view](../formats/asz-view.md) shows
the same windows to a reader.

`no_crossing_open_operation` is the gate that does real work. It does not exist for a request left
with no result at the end of a file. In the corpus that is 0 of 25,892 tool requests on the main
stream, because the runtime writes a result even for a failure. It exists for a result that arrives
after the quiet period, which in the corpus happens on 85 of the 697 gaps longer than 10 minutes,
12.2%. Committing that window would freeze the request in one window while its result lands in the
next.

**Nothing commits a segment.** Assembly proposes windows and reports `committable`, and no command
acts on it. The [model](unified-conversation-model.md) calls a segment the commit unit, but the
commit step is not implemented.

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
             │  └ tool · agent.call
             ├ message.external · context.injection
             ├ agent.launch_ack
             └ runtime.notification
```

A `tool` becomes an `agent.call` when a child stream is joined to it. Only its kind changes, so it
stays under the provider call whose response asked for it. The `agent.launch_ack` is a separate
record, and it sits in the run.

Everything else is a **typed relation** carrying its own correlation quality: `starts`, `reports`,
`ends_with`, `follows`, `summarizes`, `in_segment`. Cross-stream flow is never containment, which is
what stops a rendered conversation repeating every subagent's work inside its parent.

A tool's request and result are not joined by a relation. The tool node references the request
record and, once the join finds it, the result record. Those references are the join. Its quality
is the node's `result_join` attribute. An edge would say the same thing again, and its far end would
be a record with no node of its own. So assembly never writes the `result_of` relation that the
[Glossary](glossary.md#relations) lists.

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
no wall-clock time, so the same landed evidence and the same parser and policy versions reproduce
the same bytes and the same digest. Anything mutable or temporal lives in `conversation.state`,
outside every digest.

A parse builds a round in three steps. It fixes its watermark at the sequence the index has
reached. It assembles the whole session up to that watermark. It then compares the result with the
fold of the chain and writes only the difference:

- An entity assembled unchanged is left out, because absence means unchanged.
- A node or relation the fold holds and the assembly no longer produces gets a tombstone.
- An open unresolved entry the assembly no longer produces is superseded as `resolved`.

The comparison ignores an entity's revision and its frame type. The revision is the round number,
and only the writer sets the frame type, so comparing either would make every entity look changed
in every round. Because the whole session is assembled each time, duplicate removal always sees the
first copy. A repeated record therefore cannot become a second node in a later round. A round over
the byte budget is assembled again over a narrower window, as
[`max_round_bytes`](../setup/configuration.md#parse) describes.

**The watermark is fixed before assembly, and assembly reads nothing past it.** Deciding it
afterwards would let a round hold nodes drawn from evidence that its own header and input digest do
not cover. A collector writing at the same time makes that likely. A parse also refuses to run when
the chain already covers more landed evidence than the index holds.

**A round is written whenever landed evidence advanced**, even when no entity changed. Such a round
has no entity frames and only moves `through_seq`. Without it, the next parse would read the same
evidence again and again, and "no round" would mean both "nothing new arrived" and "nothing new
mattered". Records landed twice after a crash are one such case. The chain test
`TestInterruptedPassRepeatsRatherThanLoses` checks that they move the watermark with no node,
relation or tombstone. The scenario `replay` checks the same for a repeated block.

**The fold of rounds 1 to N equals one full assembly of the landed evidence through round N's
`through_seq`.** The full assembly is the reference, because it is simple and plainly correct.
Where the two disagree, the fold is wrong. Every [scenario](../guides/scenario.md#check) checks
this in both formats, on every node's kind, parent and attributes and on the number of relations.
It holds under any round budget, because the budget changes where the chain is cut, never what it
says. A chain test builds one session as a single round and again under a one-byte budget, one round
per landed file, and the two folds hold the same entities. The rule does not say that the fold of
round N+1 extends the fold of round N. Later evidence replaces entities that earlier rounds wrote.

**Rounds and segments are separate.** The round is the unit of appending. The segment is a proposed
commit window. A segment is a node like any other, so when a later Talk turns the open window into a
candidate, that change is an ordinary node revision in whatever round the parse writes. There is no
separate mechanism for it, and nothing freezes a segment today, as [Segments](#segments) says.

The page that `asz view` serves folds the same chain in the same way, every round in order.
[asz.view](../formats/asz-view.md) says what it does with a broken chain, and when it folds again.
A fold as of an earlier round, one entity's history across rounds, and a fold that takes rounds as
they arrive are not implemented. The rounds hold enough to support each of them.

`asz verify` checks every round against the round before it and against the landed files, as
[Session Flow](../formats/session-flow.md#reading-it) lists. One of those checks is not about
digests: the landed sequences the rounds consumed must be contiguous. It catches skipped evidence
that no digest would reveal.

## Qualification

Nothing is presented as observed execution unless it was observed.

Every join carries a correlation quality: `exact_unique`, `exact_ambiguous`, `strong_inference`,
`weak_inference`, `unresolved`, `conflict`. An exact identifier does not guarantee a unique match —
where several candidates share a key the relation stays `exact_ambiguous` and the assembler does not
choose one.

What could not be resolved is carried as data. An assembler that drops what it could not resolve
presents a partial conversation as a complete one.

A count of what could not be resolved also needs the number it is out of. Without it, a reader
cannot tell a small gap from a large one. The rounds do not carry that number. An unresolved entry
records its kind, its reference, a reason and a state. Only [`asz parse`](../setup/command-line.md#parse)
prints the number, and only for tool calls and child launches.

The assembler writes seven kinds of unresolved entry:

| Kind | Written when |
| --- | --- |
| `tool_result` | no result landed for a tool use, or several results carry its id |
| `tool_use` | a result landed, and the request it answers never did |
| `child_stream` | a child was announced, and its stream has not landed |
| `notified_child` | a notification names a child whose stream has not landed |
| `spawn_call` | a child, or a batch of children, names a launch call that never landed |
| `spawn_of_child` | a child stream landed, and no landed record names the call that started it |
| `epoch_continuation` | a reset names a record that is not in the landed data |

Every entry is written `open`, and it resolves when the missing evidence lands. `child_stream` and
`notified_child` are routine in a live session, because a child can be announced before its stream
lands. While either is open, no segment is committable. A child in a batch that never reports a
result is normal, and it is not recorded.

The assembler makes one change of state. An open entry it no longer produces is superseded as
`resolved`, and [the fold](../formats/session-flow.md#the-fold) keeps it. The scenario
`unfinished-tool` checks a `tool_result` going from open to resolved. The format also defines
`terminal`, for a producer with evidence that a reference will never resolve, such as a pruned
source. The current assembler never writes it. Nothing counts rounds either. "Still open after N
rounds" measures how often the chain was built, not the data, which is why the state has three
values and not a counter.
