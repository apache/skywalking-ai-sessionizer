# asz.view

`asz.view` is one conversation, rebuilt from its Session Flow and its Session Data, as one document:
everything a viewer renders, and nothing a viewer must compute. It is the final form of a
conversation, the one a page draws and the one a person reads. Every talk is a tree of runs and
steps with the text the referenced records carry, and the streams, the segments, the relations,
the rounds and the files sit beside them, each verified. The evidence is inside it once, so a
viewer never opens a `.sd` file; the `ref` on a node is a citation, not a pointer to fetch.

It is never a file that the project writes. Package `pkg/sessionview` defines and owns the shape,
and three things produce it from the same code:

| How | What you get |
| --- | --- |
| `asz conversation -json ID` | the document on standard output, indented |
| `asz conversation -yaml ID` | the same document rendered as YAML, with the same keys in the same order |
| `asz view`, at `/api/c/{id}/view` | the document as the page's own response, built once per fold |

A server that holds the same `.sd` and `.sf` files, such as the SkyWalking OAP, builds the same
document and answers a conversation query with it. Every reader shares the shape, and a change to
it is a change to the version: a 1.x adds keys and never removes or renames one; a 2.0 may do
either.

The document is JSON. Keys are `snake_case`, as in the two source formats, and are written in the
order this page lists them. Times are unix milliseconds, read from the `.sd` record a node
references; a view is read and never digested, so it carries no RFC 3339 strings. The same head
round over the same files gives the same document, so one built by `asz view` and one built by
another server compare equal as documents.

**A complete example.** [asz-view-example.yaml](asz-view-example.yaml) is the whole document for
the fixture session of the format pages, three talks across a main stream and a child agent, a
tool, a synthetic error and a context reset, exactly as `asz conversation -yaml` prints it. It is
generated from the scenario `tests/scenarios/fixture.yaml` by `make asz-view-example`, and a test
fails when the committed file no longer matches what the code produces.

## Top level

```json
{"format":"asz.view","version":"1.0",
 "conversation":"0438c73b-…","sessions":["0438c73b-…"],
 "head":{"round":4,"digest":"2e77…"},"parser":"v1","policy":"v1+idle=10m0s",
 "summary":{"title":"Check otel-rules support for meter mal","state":"verified","problems":[],
            "talks":357,"steps":16121,"streams":132,"segments":50,"rounds":4,"unresolved":0,
            "from":1786033245531,"to":1786502191749,
            "kinds":{"llm.call":4656,"tool":5606,…},"relation_types":{"in_segment":346,…},"quality":{"exact_unique":338,…}},
 "rounds":[{"round":1,"digest":"6d2d…","previous":null,"from_seq":1,"through_seq":5,"input_digest":"…",
            "from_time":1786033245531,"through_time":1786322168715,"verified":true}, …],
 "files":[{"file":"0438c73b-…/streams/main/transcript-…-000002.sd","format":"sd","kind":"transcript",
           "seq":2,"round":null,"stream":"main","run":null,"lines":1017,"bytes":2097000,"digest":"9a39…",
           "from_time":1786033290556,"through_time":1786036000000}, …,
          {"file":"_conversations/0438c73b-…/rounds/r000001-6d2d93b21f79.sf","format":"sf","kind":"round",
           "seq":null,"round":1,"stream":null,"run":null,"lines":3575,"bytes":1026788,"digest":"f3fe…",
           "from_time":1786033245531,"through_time":1786322168715}],
 "streams":[{"id":"stream/main","name":"main","role":"main","label":"","parent":"","records":10728,
             "steps":7310,"talk":"talk/main/95a1…","named_by":"","opened_by":[]}, …],
 "segments":[{"id":"segment/at_2_7","state":"candidate","committable":true,"talks":24,"from":1786033290556,"to":1786039329174}, …],
 "talks":[{"id":"talk/main/95a1…","kind":"talk","parent":"epoch/main/0","stream":"main","at":1786033290556,
           "ref":{"seq":2,"row":7},"attrs":{"loops":1,"runs":1,"trigger":"external"},
           "label":"for meter mal, …","reply":"## Short answer…","runs":1,"steps":132,"tools":51,
           "from":1786033290556,"to":1786033717089,"segment":"segment/at_2_7",
           "children":[…],"edges":[{"type":"in_segment","other":"segment/at_2_7","dir":"out","quality":"exact_unique","via":"activity window"}]}, …],
 "relations":[{"id":"rel/starts/…","type":"starts","from":"tool/toolu_…","to":"stream/a1…","quality":"exact_unique",
               "via":"parent tool result","evidence":[{"seq":1,"row":12}]}, …],
 "unresolved":[]}
```

| Key | Value |
| --- | --- |
| `format`, `version` | `asz.view`, `1.0`. A reader that does not know the version stops here. |
| `conversation`, `sessions` | the conversation id, and the sessions that contributed to it, from the fold's session nodes; one session, equal to the conversation id, for the Claude Code adapter |
| `head` | `round` and `digest` of the newest round the document was folded to |
| `parser`, `policy` | from the head round's header |
| `summary` | `title`; `state`, one of `verified`, `incomplete` when a round or a file is missing, `mismatch` when a digest failed; `problems`, one line each, empty when verified; the counts `talks`, `steps`, `streams`, `segments`, `rounds`, `unresolved`; `from` and `to`, when the session began and its last activity, from the session node; and `kinds`, `relation_types` and `quality`, the fold sized by node kind, by relation type and by how well each relation is known |
| `rounds` | one per round, in order: `round`, `digest`, `previous` (null on round 1), `from_seq`, `through_seq`, `input_digest`, `from_time`, `through_time` (the record time range of the files the round consumed, null when none carries a time), `verified` |
| `files` | one per `.sd` file, then one per round: `file` (its path on the wire), `format` (`sd` or `sf`), `kind`, `seq` or `round`, `stream` or `run`, `lines`, `bytes`, `digest`, `from_time`, `through_time`. Absent values are null. Together with `rounds`, this is exactly what a rebuild needs. |
| `streams` | one per execution stream: `id`, `name`, `role` (`main` or `child`), `label`, `parent`, `records`, `steps`, `talk`, `named_by`, and `opened_by`, every step the assembler could tie to the start of the stream as `{step, stream, talk, quality}`; several means it did not choose, and neither does a view |
| `segments` | one per activity window: `id`, `state`, `committable`, `talks`, `from`, `to` |
| `talks` | one tree per talk, in time order. See the node below |
| `loose` | the runs and steps no talk contains, as trees from their highest such ancestor: a child's output the fold parented to the session because the child's stream opened no talk, for instance. Empty for most conversations. With `talks`, it holds every run and step of the fold, so the document covers the whole session |
| `relations` | one per relation of the fold: `id`, `type`, `from`, `to`, `quality`, `via`, `evidence` |
| `unresolved` | one per reference the assembler could not resolve, open or since resolved: `id`, `kind`, `ref`, `reason`, `state` |

Verification is content, not an error. A gap in the chain or a failed digest is written into
`summary.state` and `summary.problems`, each round says whether it verified, and the rest of the
document holds whatever could still be folded: the fold stops before a missing or broken round,
`head` names the last round it reached, and the rounds after the gap are listed and not verified.
A viewer shows the problem; it never gets an error instead of a document. Only a chain with no
usable round at all is an error, because there is nothing to show.

## A node in `talks`

```json
{"id":"tool/toolu_01KH…","kind":"tool","parent":"call/msg_011C…","stream":"main","at":1786033295667,
 "ref":{"seq":2,"row":13,"block":0},"refs":[{"seq":2,"row":13,"block":0},{"seq":2,"row":14,"block":0}],
 "attrs":{"name":"Bash","result":"available","result_join":"exact_unique","timing":"unavailable"},
 "text":"{\"command\":\"ls …\"}","state":"available","bytes":145,"flags":["finished"],
 "name":"Bash","result":"…","result_state":"available","result_bytes":2048,
 "request_to_result_ms":1312,"request_to_result_join":"exact_unique",
 "edges":[{"type":"starts","other":"stream/a1…","dir":"out","quality":"exact_unique","via":"parent tool result"}],
 "children":[…]}
```

| Key | Value |
| --- | --- |
| `id`, `kind`, `parent`, `stream`, `attrs` | the node as the fold holds it; `kind` is one of the node kinds of Session Flow |
| `at` | when its record happened, from the record; `0` when nothing observed it |
| `ref`, `refs` | the record it stands on and every record it covers, as `{seq, row, block}`, kept so a viewer can show the evidence |
| `text`, `state`, `bytes` | the part the node stands on: its readable text, clipped to the longest prefix of whole characters within 2,000 bytes, whether the content is `available`, and its full size. For a `data` part the text is the data as compact JSON. A reader wanting the whole record reads it by address. |
| `usage`, `flags`, `dropped` | what else the referenced record says, copied once: on an `llm.call`, the token counts `in`, `out`, `cache_read`, `cache_write` from the one record `usage_at` names, never a sum over fragments; the record's `flags`; and its `dropped` list, so a viewer can say what was left out and why |
| a talk adds | `label`, `reply` (its last assistant message, clipped the same way), `runs`, `steps`, `tools`, `from`, `to`, `child`, `segment` |
| a tool or agent call adds | `name`, `failed`, `result`, `result_state`, `result_bytes`, `request_to_result_ms` and `request_to_result_join`, the time from the request record to the result record where the assembler joined them exactly |
| a `turn.duration` step adds | `duration_ms`, `duration_measured_by` |
| `children` | containment, in record order: a talk holds runs, a run holds steps, a call holds what it produced |
| `edges` | every relation touching the node, in both directions, as `{type, other, dir, quality, via}`, ordered by relation id and then direction, so a viewer draws cross-stream flow without searching `relations` and the same fold gives the same list |

Keys a node has no value for are absent, not null. Nothing in a document is inferred beyond what
the fold and the records say. Where the fold says `unavailable`, the document says it too.

## Rendering the whole conversation

The document is complete: a viewer draws every view of a conversation from it and fetches
nothing else, however many rounds the session was parsed in. A session landed and parsed in three
stages has three rounds and its landed files cut at each stage, and the document built from the
head holds every round, every file, and every talk, run and step, exactly as one built from a
single parse would; the scenario `three-rounds` checks that at each stage, and every scenario
checks the property `view_covers_the_session` at its end. This is how each view reads it.

| View | Read |
| --- | --- |
| **Transcript** | `talks`, in order. Each talk's `label` is the person's input and its `reply` the last assistant message; its `children` are the runs, a run's children the steps, and a call's children what it produced: thinking, messages, tools. `text` is what to show for a step, `name` and `result` for a tool, `usage` on a call. `loose` holds whatever no talk contains, and is usually empty. |
| **Flow timeline** | every node of every tree by `at`, with `kind` and `stream`; a node with `at` of `0` was never observed at a time and is placed by its position. |
| **Cross-stream flow** | `edges` on a node, and `relations` as the whole list: `starts` from an agent call to the child's `stream`, `reports` from the notification that resumed the parent, `ends_with` from a stream to the child's output, `follows` between epochs across a reset, `summarizes` from a summary to its boundary, `in_segment` from a talk to its window. Containment never crosses a stream; a child's work is under the child. |
| **Streams and segments** | `streams` with `role`, `label`, `parent` and `opened_by`, the step that started each; `segments` with the span of the talks placed in them. |
| **Evidence** | a node's `ref` and `refs`, `{seq, row, block}`, name the record and the part it stands on. The text is already on the node, clipped to 2,000 bytes with the full size in `bytes`; only a reader that wants the whole of a longer part goes to the record, by that address, in `files`. |
| **Verification** | `summary.state` and `summary.problems`, every round's `verified`, and every file's `digest`, `lines` and `bytes`. A gap or a failed digest is content here, never an error in place of the document. |
| **Counts and time** | `summary`: the counts a list shows, the session's `from` and `to`, and the fold sized by `kinds`, `relation_types` and `quality`. |

Nothing in a document is inferred beyond what the fold and the records say. Where the fold says
`unavailable`, the document says it too, and a viewer shows that word rather than a guess.

## The YAML rendering

`-yaml` is a rendering of the JSON, not a second format. It is produced from the JSON, so the keys
are the same and in the same order; mappings are blocks; scalars are plain, and quoted only where
YAML would otherwise misread them, so `version` is `"1.0"` and a title with a colon is quoted; an
empty map is `{}`; a text with line breaks is a block scalar. Reading the YAML back gives the same
values as the JSON.

## Reading it

`asz view` serves the document at `/api/c/{id}/view` and builds it once per fold, so a second reader
pays nothing until a new round arrives. `asz conversation -json ID` prints the same document to
standard output, and `-yaml` prints it as YAML, for a terminal or a diff; the page stays the page.
The largest conversation measured, 357 talks and 16,121 steps, is 19 MB as one document and was
built in 0.7 s.
