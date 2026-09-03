# Session Data

Session Data is what was actually in a conversation, in one shape regardless of which agent
produced it. A landed file has the extension `.sd`. Its first line is a header carrying everything
constant for the file. Every line after it is one source record, converted, with its content broken
into parts named for what they are: a message, a thought, a call, its result.

The conversion happens once, while the source is read, and never again. A runtime's vocabulary
stops at its adapter. A reader is handed parts, so there is no runtime shape for it to reach into.
A dialect that meets something it cannot describe keeps the bytes verbatim in an `unknown` part
rather than guessing or dropping them.

The format is public. Package `pkg/sessiondata` defines it, and a third-party adapter produces it.
The schema is `sd/1`.

## Header

```json
{"h":1,"schema":"sd/1","seq":2,"at":"2026-09-01T12:56:03.1818Z","kind":"transcript",
 "adapter":"claude-code-local/0.1.0","dialect":"claude-code/1",
 "src":"-Users-me-github-skywalking/0438c73b-….jsonl","session":"0438c73b-…","stream":"main"}
```

| Field | Meaning |
| --- | --- |
| `h` | envelope version, `1` |
| `schema` | `sd/1` |
| `seq` | the landed sequence number, monotonic per session |
| `at` | when the file was collected |
| `kind` | what it was collected from: `transcript`, `agent_meta`, `journal`, `workflow_manifest`, `workflow_script`, `provider_body`. `otlp_log` and `otlp_span` are reserved for a push transport. |
| `adapter` | how the records were acquired, with its contract version |
| `dialect` | whose schema they were read as. A push receiver and a local reader for one runtime share a dialect and nothing else. |
| `src` | the source, relative to the adapter's root |
| `session`, `stream`, `batch` | the session, the execution stream (`main` or an agent id), and the group of children a workflow run started |

## Record

```json
{"ord":12,"off":25086,"sha":"4fed1b624096","bytes":1524,
 "id":"c8b85d73-…","parent":"a5f0a42d-…","call":"msg_011Cdm…",
 "from":"agent","time":"2026-08-06T16:57:22.652Z","flags":["finished"],
 "usage":{"in":2,"out":331,"cache_read":20729,"cache_write":19709},
 "parts":[{"k":"call","id":"toolu_015jKz…","name":"Bash","data":{"command":"make check"},
           "state":"available","bytes":181}]}
```

Two halves. The record level carries provenance and the identifiers structure is built from, in
role names. The part level carries what the content is.

| Field | Meaning |
| --- | --- |
| `ord`, `off`, `sha`, `bytes` | where the record was in the source: line number, byte offset, digest of the source bytes and their size. The bytes themselves are not kept; the digest stays so provenance is provable, and a record that claims a source it did not come from is detectable. |
| `id`, `parent` | the record's own identity and its containment parent |
| `call` | the provider call this record is a fragment of |
| `run` | the agent loop it belongs to, one per trigger |
| `continues` | on a reset boundary only, the record the new context resumes from |
| `tool`, `child`, `batch`, `started_by` | joins the runtime states outside the content: the call a sidecar answers, the child stream a record names, the group it belongs to, the stream that started this one |
| `label` | a name the runtime gave something. The only naming evidence in the data. |
| `from` | who produced it: `agent`, `external`, `runtime`. A record's type is not what a record is; most records that look like a person are a tool answering. |
| `time`, `trigger`, `flags` | when, what started the loop, and states such as `finished` |
| `usage` | what the provider reported: input, output, cache read and cache write tokens. Meaningful only where the call finished. |
| `parts` | the content |
| `dropped` | what the conversion chose to leave out, with its size and the reason |

Empty identifiers are common and mean the runtime supplied none. Nothing is inferred to fill
them.

## Parts

| Kind | Is | Carries |
| --- | --- | --- |
| `text` | readable text | `text` |
| `reasoning` | the model's own reasoning | `text` when the runtime kept it |
| `call` | a request to run something | `id`, `name`, `data` |
| `result` | what a call returned | `of`, `text` or `data`, `failed` |
| `media` | an image or a document | `media`, `data` |
| `data` | structure that is not prose: a record the runtime keeps for itself, a manifest | `data` |
| `unknown` | content the dialect could not describe | the raw bytes in `data` |

Every part carries `state` and `bytes`. `state` is one of `available`, `truncated`, `redacted`,
`omitted` or `unavailable`, and `bytes` is the size of the original even when the part holds less.
A reader is always told how much of the original it has. One tool result in the measured corpus
was 1.1 MB, and the share of a session that is tool output ran from 17% to 84%, so a reader is
routinely shown part of something and has to know it.

`failed` is absent, not false, when the runtime said nothing. Most results carry no error flag,
and reading that as success would report something the runtime never claimed.

The set of kinds is small because the thing described is small. Measured across a corpus of 3,032
Claude Code files, six content shapes exist and four cover 99.99% of records.

## Dropped

```json
"dropped":[{"what":"reasoning signature","bytes":588,"why":"a provider verifies it; a reader cannot read it"}]
```

`dropped` exists so a loss is stated rather than silent. It lists only what the dialect understood
and chose not to carry. Everything it did not understand travels as an `unknown` part, so a later
version of the dialect can interpret it without collecting again.
