# Session Flow

Session Flow is the conversation structure: the talks, streams, runs, steps and relations that
[assembly](../concepts-and-designs/conversation-assembly.md) builds from landed records. It is
published as an append-only chain of immutable rounds. A round file has the extension `.sf`, and
the conversation is the fold of every round. There is no other state.

The format is public. Package `pkg/sessionflow` defines it, and a consumer reads it. The schema is
`sf/1`.

## A round

One round is one file of JSON lines. Every line carries `t`, its frame type, and the frames come in
a fixed order: one `header`, then any number of `node`, `relation` and `unresolved` frames, then
one `commit`.

```json
{"t":"header","schema":"sf/1","conversation":"1213…","session":"1213…","round":1,
 "from_seq":1,"through_seq":118,"input_digest":"48d8…","parser":"v1","policy":"v1+idle=10m0s"}
{"t":"node","id":"ack/1/1338","revision":1,"kind":"agent.launch_ack","parent":"run/…","ref":{…}}
{"t":"relation","id":"…","revision":1,"type":"starts","from":"…","to":"…","quality":"exact","evidence":[…]}
{"t":"unresolved","id":"…","revision":1,"kind":"tool","ref":"toolu_…","reason":"…","state":"open"}
{"t":"commit","digest":"d829…","counts":{"nodes":1338,"relations":52,"unresolved":0}}
```

### Header

| Field | Meaning |
| --- | --- |
| `conversation`, `session` | the conversation this chain belongs to, and the session the round was assembled from |
| `round` | counts from 1 |
| `previous` | the digest of round N-1. Empty only for round 1. This is a dependency, not audit metadata: a round is built on the fold of the round it names. |
| `from_seq`, `through_seq` | the landed sequence range this round consumed |
| `input_digest` | binds the round to the landed evidence it read. It is chained rather than recomputed, the previous input digest hashed with the digests of the newly landed files, so producing it stays proportional to new data. |
| `parser`, `policy` | the interpretation versions. A change to either that alters meaning starts a new chain. |

A header carries no wall-clock time. A round's bytes must be reproducible from its inputs, so the
same landed range, the same previous digest and the same parser version yield the same digest.
When a round was produced lives outside every digest, in `conversation.state`.

### Entities

Nodes, relations and unresolved references share one envelope: an `id`, a `revision` and an
optional `tombstone`. An id comes from stable evidence in the landed records, never from position,
so an id cannot shift when an earlier source is backfilled later.

| Frame | Fields |
| --- | --- |
| `node` | `kind`, `parent` for containment, `stream`, `ref` or `refs` pointing at the landed records that back it, and `attrs` |
| `relation` | `type`, `from`, `to`, `quality`, `via`, and `evidence`. A typed edge that is not containment. Cross-stream flow is a relation, never containment: a child's work stays under the child. |
| `unresolved` | `kind`, `ref`, `reason`, and `state`: `open`, `resolved` or `terminal` |

A reference into the landed data is a landed sequence, a row, and optionally a content block. That
is the whole address; a reader turns it into a moment through the index and into content through
the `.sd` record.

### Commit

`digest` covers every preceding line of the file, so a round verifies itself, and it is the value
the next round names as `previous`. `counts` lets a reader detect a truncated round without folding
it.

## The fold

The conversation as of round N is the fold of rounds 1 through N, applied in order.

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

`asz conversation ID` folds a chain and prints what it holds. `asz verify` recomputes every commit
digest. The node kinds and relation types are the vocabulary of the
[Unified Conversation Model](../concepts-and-designs/unified-conversation-model.md), and the
[glossary](../concepts-and-designs/glossary.md) fixes each word.
