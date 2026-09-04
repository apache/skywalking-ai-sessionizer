# Conversation View

The Conversation View is one conversation, rebuilt from its Session Flow and its Session Data,
as one document. It is what a reader wants: every talk as a tree of runs and steps, with the text
the referenced records carry, plus the streams, the segments, the relations, and what the
conversation was built from.

It is never a file. `asz view` builds it in memory from the `.sf` rounds and the `.sd` files and
serves it as one response at `/api/c/{id}/view`. A server that holds the same files, such as the
SkyWalking OAP, builds the same document and answers a conversation query with it. Package
`pkg/sessionview` defines and owns the shape. Every reader shares it, and a change to it is a
change to the schema, `cv/1`.

Times in a view are unix milliseconds, because a view is read and never digested. Session Data
and Session Flow carry RFC 3339 strings, because their bytes are.

## The document

```json
{"schema":"cv/1","conversation":"0438c73b-…","session":"0438c73b-…",
 "title":"Check otel-rules support for meter mal","from":1786033245531,"to":1786502191749,
 "head":{"round":4,"digest":"2e77…","through_seq":311,"input_digest":"…","parser":"v1","policy":"v1+idle=10m0s"},
 "rounds":[{"round":1,"digest":"6d2d…","from_seq":1,"through_seq":5,"input_digest":"…",
            "from":1786033245531,"to":1786322168715,"session_from":1786033245531,"session_to":1786322168715,
            "lines":3575,"bytes":1026788,"file_digest":"f3fe…","file":"_conversations/0438c73b-…/rounds/r000001-6d2d93b21f79.sf"}, …],
 "files":[{"file":"0438c73b-…/streams/main/transcript-…-000002.sd","format":"sd","version":"sd/1","kind":"transcript",
           "seq":2,"stream":"main","lines":1017,"bytes":2097000,"digest":"9a39…","from":1786033290556,"to":1786036000000}, …],
 "counts":{"nodes":16129,"relations":703,"unresolved":0,"talks":357,"steps":16129,"streams":132,"segments":50},
 "kinds":{"llm.call":4656,"tool":5606,…},"relation_types":{"in_segment":346,…},"quality":{"exact_unique":338,…},
 "streams":[{"id":"stream/main","name":"main","role":"main","label":"","named_by":"","records":10728,
             "parent":"","talk":"talk/main/95a1…","steps":7310,"opened_by":[]}, …],
 "segments":[{"id":"segment/at_2_7","state":"candidate","talks":24,"from":1786033290556,"to":1786039329174,"committable":true}, …],
 "talks":[{"id":"talk/main/95a1…","stream":"main","label":"for meter mal, …","runs":1,"steps":132,"tools":51,
           "from":1786033290556,"to":1786033717089,"child":false,"segment":"segment/at_2_7","reply":"## Short answer…",
           "tree":{"id":"talk/main/95a1…","kind":"talk","at":1786033290556,"ref":{"seq":2,"row":7},"children":[…]}}, …],
 "relations":[{"id":"rel/starts/…","type":"starts","from":"tool/toolu_…","to":"stream/a1…","quality":"exact_unique",
               "via":"parent tool result","evidence":[{"seq":1,"row":12}]}, …],
 "unresolved":[]}
```

| Field | Meaning |
| --- | --- |
| `schema` | `cv/1` |
| `conversation`, `session`, `title` | the conversation, the session it was assembled from, and the last name the runtime gave it |
| `from`, `to` | when the session began and its last activity, from the session node |
| `head` | the fold the view was built from: the head round, its digest, how far the landed evidence was consumed, the parser and the policy |
| `rounds` | every round of the chain in order, from its header and its file. `from` and `to` are the record time range of the files the round consumed; `session_from` and `session_to` the session's range as of that round. A reader can verify the chain from this list alone: each `previous` names the digest before it. |
| `files` | every landed file of the session, with its kind, digest, size and record time range. Together with `rounds`, this is exactly what a rebuild needs. |
| `counts`, `kinds`, `relation_types`, `quality` | how large the fold is, by node kind, by relation type, and by how well each relation is known |
| `streams` | every execution stream, with the step that started it where the assembler found one. `opened_by` lists every candidate and never chooses. |
| `segments` | every activity window, with the span of the talks placed in it |
| `talks` | every talk in time order: its summary row, and `tree`, the talk as a tree of runs, steps and what a call contains |
| `relations` | every relation of the fold, with the records it stands on |
| `unresolved` | every reference the assembler could not resolve, open or since resolved |

## A node in a tree

```json
{"id":"tool/toolu_01KH…","kind":"tool","parent":"call/msg_011C…","stream":"main","at":1786033295667,
 "ref":{"seq":2,"row":13,"block":0},"refs":[{"seq":2,"row":13,"block":0},{"seq":2,"row":14,"block":0}],
 "attrs":{"name":"Bash","result":"available","result_join":"exact_unique","timing":"unavailable"},
 "name":"Bash","text":"{\"command\":\"ls …\"}","state":"available","bytes":145,
 "result":"…","result_state":"available","result_bytes":2048,
 "request_to_result_ms":1312,"request_to_result_join":"exact_unique",
 "edges":[{"type":"starts","other":"stream/a1…","dir":"out","quality":"exact_unique","via":"parent tool result"}],
 "children":[…]}
```

| Field | Meaning |
| --- | --- |
| `id`, `kind`, `parent`, `stream`, `attrs` | the node as the fold holds it |
| `at` | when its record happened, from the record; `0` when nothing observed it |
| `ref`, `refs` | the record it stands on and every record it covers, as `{seq, row, block}` |
| `text`, `name`, `state`, `bytes`, `failed` | what the part it stands on carries: the readable text, clipped to 2,000 bytes, the name of a call, whether the content is here, the full size, and whether the runtime reported a failure. A reader wanting the whole record reads it by address. |
| `result`, `result_state`, `result_bytes` | for a call, what came back, clipped the same way |
| `duration_ms`, `duration_measured_by` | for a `turn.duration` step, what the runtime measured and how |
| `request_to_result_ms`, `request_to_result_join` | for a tool, the time from the request record to the result record, only where the assembler joined them exactly |
| `edges` | every relation touching the node, in both directions |
| `children` | containment, in record order |

Nothing in a view is inferred beyond what the fold and the records say. Where the fold says
`unavailable`, the view says it too.

## Reading it

`asz view` serves a view at `/api/c/{id}/view` and builds it once per fold, so a second reader pays
nothing until a new round arrives. The largest conversation measured, 357 talks and 16,129 steps,
is about 19 MB as one document, 4.6 MB of it text.
