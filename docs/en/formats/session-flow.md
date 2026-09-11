# Session Flow

Session Flow is the conversation structure: the talks, streams, runs, steps and relations that
[assembly](../concepts-and-designs/conversation-assembly.md) builds from landed records. It is
published as an append-only chain of immutable rounds. A round file has the extension `.sf`, and
the conversation is the fold of every round. There is no other state.

The format is public. Package `pkg/sessionflow` defines it, and a consumer reads it. The schema is
`sf/1`.

There is one kind of `.sf` file, a round; on the wire and in a receiver it is `round`.

## A round

One round is one file of JSON lines. Every line carries `t`, its frame type, and the frames come in
a fixed order: one `header`, then any number of `node`, `relation` and `unresolved` frames, then
one `commit`.

```json
{"t":"header","schema":"sf/1","conversation":"1213…","session":"1213…","round":1,
 "from_seq":1,"through_seq":118,"input_digest":"48d8…","parser":"v1","policy":"v1+idle=10m0s",
 "from_time":"2026-08-06T16:21:30.556Z","through_time":"2026-08-07T02:14:51.749Z",
 "session_from_time":"2026-08-06T16:20:45.531Z","session_through_time":"2026-08-12T02:36:31.749Z",
 "title":"Check otel-rules support for meter mal","talks":357,"steps":16121,"streams":132,"segments":50,"unresolved":0}
{"t":"node","id":"ack/1/1338","revision":1,"kind":"agent.launch_ack","parent":"run/…","ref":{…}}
{"t":"relation","id":"rel/starts/tool_toolu_01Ax…/stream_a308…","revision":1,"type":"starts","from":"tool/toolu_01Ax…","to":"stream/a308…","quality":"exact_unique","via":"child sidecar","evidence":[…]}
{"t":"unresolved","id":"unres/tool_result/toolu_01Bq…","revision":1,"kind":"tool_result","ref":"toolu_01Bq…","reason":"no result landed for this tool use","state":"open"}
{"t":"commit","digest":"d829…","counts":{"nodes":1338,"relations":52,"unresolved":0}}
```

### Header

| Field | Meaning |
| --- | --- |
| `conversation`, `session` | the conversation this chain belongs to, and the session the round was assembled from |
| `round` | counts from 1 |
| `previous` | the digest of round N-1. Empty only for round 1. This is a dependency, not audit metadata: a round is built on the fold of the round it names. |
| `from_seq`, `through_seq` | the landed sequence range this round consumed |
| `input_digest` | binds the round to the landed evidence it read. It is chained rather than recomputed, so producing it stays proportional to new data. How it is computed follows the table. |
| `parser`, `policy` | the interpretation versions. `policy` includes the idle gap, the quiet period after which a segment can be closed. asz writes `v1+idle=10m0s`, and a round made under another gap has another policy. |
| `from_time`, `through_time` | the earliest and the latest record time among the landed files this round consumed, as the runtime wrote them, in UTC. Absent when no record in the window carries a time. |
| `session_from_time`, `session_through_time` | the session's own range as of this round: when it began, and its last activity so far. The `session` node carries the same pair as `from_time` and `through_time` in its attributes; the header repeats it so a reader of the header alone learns it without folding. |
| `title`, `talks`, `steps`, `streams`, `segments`, `unresolved` | what a list of conversations shows, as of this round: the session's title and the fold's counts of talks, steps, streams, segments and open unresolved references. A receiver lists conversations off the newest round's header, or off the attributes a sender copies from it, and never folds. |
| `changes`, `lines_added`, `lines_removed` | the workspace change records the landed files carry up to `through_seq`. `changes` counts the distinct records: one for each tool call a producer watched for file changes, whether or not the call changed a file. The other two are the lines those records' diffs add and remove. |
| `llm_calls`, `subagents`, `bash_runs` | the fold's `llm.call` nodes, its streams whose role is `child`, and its `tool` and `agent.call` nodes named `Bash`. A call that started a child agent becomes an `agent.call` and keeps its name, so `bash_runs` does not fall when the child's transcript lands. |

Each of the six counts in the last two rows is absent from a round written before that count
existed. A reader takes an absent count as unknown, never as zero. The list that
[`asz view`](../setup/command-line.md#view) serves shows a dash for it.

The input digest is a SHA-256 in lowercase hex:

```text
input_digest = hex( SHA-256( previous || 0x00 || d1 || 0x00 || d2 || ... ) )
```

- `previous` is the previous round's `input_digest` as hex text. It is empty for round 1.
- `d1`, `d2` and the rest are the SHA-256 of each whole landed file with a sequence from `from_seq`
  through `through_seq`, in lowercase hex. They are sorted as strings first, because directory
  listing order is not stable.

A chain holds one interpretation, so its parser and policy never change. The parser refuses to
extend a chain built by another parser or policy, and [the fold](#the-fold) refuses to mix them. To
move a conversation to a new parser or policy, remove `_conversations/<id>/` and parse again. The
landed files rebuild it, as [Storage Root](storage-root.md#retention) says.

A header carries no wall-clock time. A round's bytes must be reproducible from its inputs, so the
same landed range, the same previous digest, and the same parser and policy versions yield the same
digest. When a round was produced lives outside every digest, in `conversation.state`. Record times
are different: the runtime wrote them into the landed files, so they are evidence, and the time
fields above reproduce with the round.

The order of the frames is part of the bytes. asz writes the header, then every node, then every
relation, then every unresolved entry, each group sorted by id, and then the commit. Without the
sort, two runs over the same input could order the frames differently and produce different
digests.

### Entities

Nodes, relations and unresolved references share one envelope: an `id`, a `revision` and an
optional `tombstone`. An id comes from stable evidence in the landed records, never from position,
so an id cannot shift when an earlier source is backfilled later.

| Frame | Fields |
| --- | --- |
| `node` | `kind`, `parent` for containment, `stream`, `ref` or `refs` pointing at the landed records that back it, and `attrs` |
| `relation` | `type`, `from`, `to`, `quality`, `via`, and `evidence`. A typed edge that is not containment. Cross-stream flow is a relation, never containment: a child's work stays under the child. |
| `unresolved` | `kind`, `ref`, `reason`, and `state`: `open`, `resolved` or `terminal`. [Qualification](../concepts-and-designs/conversation-assembly.md#qualification) lists the kinds asz writes and when it writes each. |

An id joins its parts with `/`. An empty part is left out, and a `/` inside a part becomes `_`, so
the run in talk `talk/main/8d53…` is `run/talk_main_8d53…/8d53…`. The parts are joined rather than
hashed, so an id stays readable in a round file and in an error message.

| Entity | Id |
| --- | --- |
| a node with an identity from the runtime | a prefix and that identity, such as `tool/<tool use id>` or `stream/<stream name>`. Where the identity is not unique across streams, the stream is a part as well, as in `talk/<stream>/<prompt cycle id>`. A chain holds one session, so no other id needs the session as a part. |
| a node with no identity from the runtime | a prefix and the landed location of its evidence: `<prefix>/<seq>/<row>`, with `:<block>` added for one content block, such as `ack/1/1338`. A landed record never moves, so neither does the id. |
| a relation | `rel/<type>/<from>/<to>`. It is keyed by what it joins, not by the evidence that showed it, so one edge observed twice is one relation. The assembler merges the evidence and keeps the stronger [quality](../concepts-and-designs/conversation-assembly.md#qualification). Once either observation says `conflict`, the relation stays `conflict`, because more agreement does not make a disagreement untrue. |
| an unresolved reference | `unres/<kind>/<ref>`. It is keyed by what is missing, so the revision that resolves it has the same id. Keyed by the round that noticed it, every round would open a new one. |

A node's prefix is short, and it is not always the node's `kind`. An `llm.call` is `call/<id>`. A
tool call that started a child agent becomes an `agent.call` and keeps its `tool/<tool use id>`.
These are the prefixes asz writes:

| Kind | Id |
| --- | --- |
| `session` | `session/<session id>` |
| `stream` | `stream/<stream name>` |
| `epoch` | `epoch/<stream>/<id of the reset record>`. A stream's first epoch is `epoch/<stream>/0`. A reset record with no `id` is named by its location, `at_<seq>_<row>`. |
| `segment` | `segment/at_<seq>_<row>`, the location of its first talk's first record |
| `talk` | `talk/<stream>/<prompt cycle id>`, from its first run. A child stream is one talk, so its talk is `talk/<stream>`. |
| `run` | `run/<talk id>/<prompt cycle id>` |
| `llm.call` | `call/<provider call id>`, the `call` its fragments share |
| `tool`, `agent.call` | `tool/<tool use id>` |
| `agent.output` | `output/<stream>` |
| `message.assistant`, `message.synthetic` | `msg/<seq>/<row>:<block>` |
| `thinking` | `think/<seq>/<row>:<block>` |
| `message.external` | `input/<seq>/<row>` |
| `context.injection` | `inject/<seq>/<row>` |
| `agent.launch_ack` | `ack/<seq>/<row>` |
| `runtime.notification` | `notify/<seq>/<row>` |
| `epoch.boundary` | `boundary/<seq>/<row>` |
| `epoch.summary` | `summary/<seq>/<row>` |
| `error.api` | `error/<seq>/<row>` |
| `turn.duration` | `turn/<seq>/<row>` |
| `control.command` | `command/<seq>/<row>` |
| `control.notice` | `notice/<seq>/<row>` |

A reference into the landed data is a landed sequence, a row, and optionally a content block. That
is the whole address. A reader takes both the content and the time from the `.sd` record it names.
The record's `time` field is when the runtime says it happened. The page that `asz view` serves
never uses the index for either, as [Storage Root](storage-root.md#the-index) says, so a root
without its index still shows its times. Apart from the `session` node's range, a node carries no time of its own.

Content is referenced, never inlined. A provider call's input is everything before it in its
context. Copying each call's input into its node would store every message again in each later
call, so storage would grow with the depth of the conversation rather than with its content. With
references only, the rounds of a 48-session root measured 105 MB, 8.9% of the source records, on
2026-09-03, as [Storage Root](storage-root.md#size) shows.

### Commit

`digest` covers every preceding line of the file, so a round verifies itself, and it is the value
the next round names as `previous`. It is the SHA-256 of those lines, each ending in its newline, in
lowercase hex. `counts` lets a reader detect a truncated round without folding it.

## The fold

The conversation as of round N is the fold of rounds 1 through N, applied in order. A fold takes a
round only if it carries the next round number, names the digest of the round before as
`previous`, and carries the chain's `conversation` and round 1's `session`, `parser` and `policy`.
It refuses any other round, so a reader detects a mix of interpretations instead of folding through
it.

- A frame with a higher `revision` supersedes the earlier one with the same id.
- Absence in a later round means unchanged, never deleted. Removal is an explicit tombstone, which
  removes the entity from the result and leaves it in history.
- An unresolved reference that later resolves is superseded by a revision whose state says
  `resolved`. It never vanishes, because absence never means resolved.
- `terminal` means evidence says the reference will never resolve: a pruned source, a record the
  runtime never wrote. It is never inferred from elapsed rounds.

A round is never rewritten. Later evidence produces a new revision in a later round.

## On disk

```text
_conversations/<conversation-id>/
  conversation.state          head, head digest, through_seq, input digest, parser, policy
  rounds/r000001-<digest>.sf  round number and the first twelve characters of its digest
```

The rounds directory is the authority. The state file is a cache, and a crash between publishing a
round and saving state leaves a round the state does not mention, so the head is recovered by
listing the directory and the previous input digest is taken from the last round's own header.

## Reading it

`asz conversation ID` folds a chain and prints what it holds. The node kinds and relation types are
the vocabulary of the
[Unified Conversation Model](../concepts-and-designs/unified-conversation-model.md), and the
[glossary](../concepts-and-designs/glossary.md) fixes each word.

A reader rejects a round, rather than folding it, when any of these is true. A server that mirrors
the format must reject the same rounds. The rules are strict because nothing looks at a round again
once it is folded. A reader that accepts what a correct producer never writes can disagree with
another reader about what the round said.

- The first line is not a header, a second header appears, or a line follows the commit.
- A line does not decode, or its frame type is unknown.
- The header breaks one of its own rules. Its schema is not `sf/1`. It lacks `conversation`,
  `session`, `parser`, `policy` or `input_digest`. Its `round` is 0. Its `previous` is missing
  after round 1, or present on round 1. Its `from_seq` is 0, or its `through_seq` is below
  `from_seq` minus one. A `through_seq` of exactly `from_seq` minus one is valid: that round
  consumed no new evidence.
- An entity has no id, or an id appears twice in one round.
- A `revision` differs from the header's `round`.
- A reference is sequence 0 row 0, or names a sequence past `through_seq`, which the round's input
  digest does not cover.
- A relation that is not a tombstone lacks `type`, `from` or `to`.
- An unresolved entry that is not a tombstone has a state other than `open`, `resolved` or
  `terminal`.
- The commit is missing, so the round is truncated, or its `digest` or `counts` do not match what
  was read.

[`asz verify`](../setup/command-line.md#verify) checks six things for every round:

1. No round is missing before it.
2. Its `previous` is the digest of the round before.
3. Its own digest matches its bytes, and it passes every rule above.
4. Its `from_seq` is the previous round's `through_seq` plus one.
5. Every landed sequence it consumed is still on disk.
6. The input digest recomputed from those files equals its header's.

Check 4 is the one no digest can make. The digests prove that the rounds were not changed. Only
contiguous `from_seq` and `through_seq` prove that no landed evidence was skipped.
