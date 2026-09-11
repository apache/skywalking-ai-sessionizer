# Session Data

Session Data is what was actually in a conversation, in one shape regardless of which agent
produced it. A landed file has the extension `.sd`. Its first line is a header carrying everything
constant for the file, and its last line closes the file with a digest. Every line between them is
one source record, converted, with its content broken into parts named for what they are: a
message, a thought, a call, its result.

The conversion happens once, while the source is read, and never again. A runtime's vocabulary
stops at its adapter. A reader is handed parts, so there is no runtime shape for it to reach into.
A dialect that meets something it cannot describe keeps the bytes verbatim in an `unknown` part
rather than guessing or dropping them.

The format is public. Package `pkg/sessiondata` defines it, and a third-party adapter produces it.
The schema is `sd/1`.

## One record per line

Every record is one line, for two reasons.

A row is a line. A round addresses a record by the file's sequence and the record's line in it,
two integers rather than a path into nested structure. They stay valid because a landed file is
never rewritten. See [Addressing a record](#addressing-a-record).

A reader can decode one record at a time and never hold the whole file. A source record is landed
whole however large it is (see [Append cursors](storage-root.md#append-cursors)), so a line has no
fixed length. A line reader with a fixed limit, such as Go's `bufio.Scanner` at its 64 KB default,
stops at the first longer line.

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
| `kind` | what it was collected from: `transcript`, `agent_meta`, `journal`, `workflow_manifest`, `workflow_script` or `changes`. `otlp_log` and `otlp_span` are reserved for a push transport, and `provider_body` is reserved too. No adapter writes any of the three. |
| `adapter` | how the records were acquired, with its contract version |
| `dialect` | whose schema they were read as. A push receiver and a local reader for one runtime share a dialect and nothing else. |
| `src` | the source, relative to the adapter's source root, with forward slashes on every platform |
| `session`, `stream`, `batch` | the session, the execution stream (`main` or an agent id), and the group of children a workflow run started |

A reader refuses a header unless `h` is `1`, `schema` is `sd/1`, and `kind`, `session`, `src` and
`dialect` are all present. The writer in `pkg/sessiondata` fills in `h` and `schema`, and refuses
to start a file whose header fails the same check. `dialect` is required because without it nothing
says whose vocabulary the parts were read in. A later reader could not tell a shape it should
understand from one it should not.

Because the header holds what is constant, no record repeats it, and a file can be read on its own,
away from the directory it was landed in. It names its session, its stream or batch, its kind and
its sequence.

`src` is kept because the storage root does not follow the source's layout. The root files a
session under its id and puts every child stream flat under `streams/`, keyed by agent id (see
[Storage Root](storage-root.md)). For Claude Code, that drops two things the source path holds: the
project directory a source file sits in, and the workflow directory a child's transcript sits in.
`src` is the only place both survive. It uses forward slashes on every platform, so a root reads
the same on the machine that collected it and on any other. With a record's `off` and `sha`, it
lets a reader check the record against its source for as long as the source exists.

## File kinds

One `.sd` file holds one cut of one source. The header's `kind` says which, and the records of
each kind carry a characteristic set of fields.

| `kind` | Collected from | Its records | What refers to it |
| --- | --- | --- | --- |
| `transcript` | one execution stream: the main stream or a child agent's own | the conversation itself: inputs, calls in fragments, results, injections, resets. `stream` on the header is `main` or the agent id. | almost every node of a round |
| `agent_meta` | what the runtime recorded about a child agent when it started it | one record, `from: runtime`, with `child` and `label`, and a `data` part | the child's `stream` node, for its label |
| `journal` | a workflow run's journal | one record per event, `from: runtime`, with `child` and `batch`; a `child_result` flag on the record that returns a child's value | `agent.output` nodes and `ends_with` relations |
| `workflow_manifest` | a workflow run's manifest | one record with `batch` and `label`, and a `data` part | the run's name |
| `workflow_script` | the program a workflow ran | one record whose part is `unknown`: the source is a program, not data | nothing; kept because it is part of the session |
| `changes` | the files the asz Claude Code plugin saw a tool call change, one line per call | one record per observed call, with `id`, `tool` and `time` lifted from it and the line whole as one `data` part, a `changes/1` record; no `from` and no flags, so assembly emits no node for it | nothing in a round; the view joins each to its step by `tool`. A transcript's `Edit` and `Write` results carry the same record as a second `data` part, from the runtime's own patch. See the [Claude Code plugin](../setup/claude-code-plugin.md). |

A source that is one document, not a stream of lines, lands as one record with `ord` 1 and `off` 0.
Its `sha` and `bytes` cover the whole document. The Claude Code adapter's `agent_meta`,
`workflow_manifest` and `workflow_script` files are of this kind, and a new version of the document
lands as a new file (see [Snapshot cursors](storage-root.md#snapshot-cursors)).

Files of every kind are bound by the rounds' `input_digest`, so a session travels or archives as
all of them. A session's `agent_meta`, `workflow_manifest` and `workflow_script` records carry
no time, so those files have no record time range.

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
| `ord`, `off`, `sha`, `bytes` | where the record was in the source: its line number counting from 1, the byte offset its line starts at, the first twelve hexadecimal characters of the SHA-256 of its bytes, and their size. The bytes themselves are not kept. The digest stays, so provenance is provable and a record that claims a source it did not come from is detectable. |
| `id`, `parent` | the record's own identity and its containment parent |
| `call` | the provider call this record is a fragment of |
| `run` | the agent loop it belongs to, one per trigger |
| `continues` | on a reset boundary only, the record the new context resumes from |
| `tool`, `child`, `batch`, `started_by` | joins the runtime states outside the content: the call a record refers to (a notification names the call it completes), the child stream a record names, the group it belongs to, the stream that started this one |
| `label` | a name the runtime gave something. The only naming evidence in the data. |
| `from` | who produced it: `agent`, `external`, `runtime`. A record's type is not what a record is; most records that look like a person are a tool answering. |
| `time`, `trigger`, `flags` | when, what started the loop, and states such as `finished` |
| `usage` | what the provider reported: input, output, cache read and cache write tokens. Meaningful only where the call finished. |
| `model` | the provider model the call ran on, as the runtime named it, on the records of a call. What a token count is reported under. Empty on records landed before it was kept. |
| `parts` | the content |
| `dropped` | what the conversion chose to leave out, with its size and the reason |

Empty identifiers are common and mean the runtime supplied none. Nothing is inferred to fill
them.

`child_result` is not only on a `journal` record. On a `transcript` record it marks a result that
is a child's synchronous return, which is the parent's copy of the child's output. Assembly reads
the flag only on a `journal` record. Nothing removes the parent's copy, so it stays as the result
of the call that started the child.

`asz verify` checks `ord`, `off` and `bytes` without the source. In each stream or run, across its
files of one kind, `ord` must run 1, 2, 3 with no gap, starting at 1. The first record must start
at byte 0, and each record after it at the byte after the previous record's line ends. Where the
stream has an append cursor, the records must reach the line and the byte the cursor names (see
[Append cursors](storage-root.md#append-cursors)). A skipped line, or a lost file of a source that
is a stream of lines, would otherwise show only as a shorter conversation. The Claude Code
adapter's `agent_meta`, `workflow_manifest` and `workflow_script` files are each one document with
a snapshot cursor, so a lost one is found only by the round chain (see
[Retention](storage-root.md#retention)). A repeated record is not a gap (see
[Landed files](storage-root.md#landed-files)).

### Addressing a record

A round points at a record with `{seq, row}`, and at one of its parts with `{seq, row, block}`
(see [Session Flow](session-flow.md#entities)). In Session Data terms:

| Field | Is |
| --- | --- |
| `seq` | the `seq` in the file's header |
| `row` | the record's line after the header, counting from 1 |
| `block` | the part's position in `parts`, counting from 0 |

The index numbers every part, including parts it has no use for, so a number names the same part
to the index, to a round and to a reader. A dialect writes one part for each piece of content in
the source record, in source order. Content it cannot describe goes in as an `unknown` part in the
same place, never left out. A part the adapter adds beside the content, such as the `changes/1`
record on an edit result, comes after them.

## Closing line

```json
{"t":"end","records":118,"digest":"c41e…"}
```

Every file ends with this line. `records` is the number of record lines, and `digest` is the
SHA-256, in hexadecimal, of every byte before this line: the header and every record, each with its
newline. The writer computes it as it writes, so a file can be checked on its own.

A reader refuses a file with no closing line, or one whose digest or count does not match. A file
cut short would otherwise read as a shorter conversation, with nothing saying so. `asz verify`
reads every landed file this way.

## Parts

| Kind | Is | Carries |
| --- | --- | --- |
| `text` | readable text | `text` |
| `reasoning` | the model's own reasoning | `text` when the runtime kept it |
| `call` | a request to run something | `id`, `name`, `data` |
| `result` | what a call returned | `of`, `failed`, and `text`, `data` or both |
| `media` | an image or a document | `media`, `data` |
| `data` | structure that is not prose: a record the runtime keeps for itself, a manifest | `data` |
| `unknown` | content the dialect could not describe | the bytes in `data` as one JSON string, the bytes themselves or their base64 when `encoding` is `base64`, and the reason in `text` |

Every part carries `state` and `bytes`. `state` is one of `available`, `truncated`, `redacted`,
`omitted` or `unavailable`, and `bytes` is the size of the original even when the part holds less.
A reader is always told how much of the original it has.

No fixed rule could decide how much of a part to keep or show, because tool output varies too much.
The measured corpus is 62 Claude Code sessions: 3,032 files and 1,100.1 MB of source records. One
tool result in it was 1.1 MB. Over its 28 sessions larger than 1 MB, the share of a session that is
tool output ran from 17.3% to 83.5%, with a median of 35.1%. A rule tuned to the median is wrong by
a factor of two at both ends. So every part states its own size and state, and a reader decides.
No built-in adapter writes `truncated` or `omitted`, so no part they write is clipped. Clipping is
left to a reader, such as the [asz.view](asz-view.md#a-node-in-talks) document, which clips a
node's text and gives the full size beside it. A part is still not always all the runtime gave.
The Claude Code dialect leaves some content out with no `dropped` entry. Its
[Session Data mapping](../adapters/claude-code.md#session-data-mapping) lists these as known gaps.

An `unknown` part's `text` is not content. It is the dialect's reason for not describing the bytes,
such as `the record is not valid JSON` or `block type "x" is not one this dialect describes`. Its
`state` is `available`, because every byte is kept, and `bytes` is their size. A reader that shows
`text` as the message would show the reason in its place.

An `unknown` part's `data` is always one JSON string, and it holds the bytes in one of two forms.
When the bytes are valid UTF-8, the string is the bytes themselves, and the part has no `encoding`.
A JSON string cannot hold bytes that are not valid UTF-8: Go's JSON encoder writes U+FFFD in their
place, and the byte is lost. So when any byte is not valid UTF-8, the string is all of the bytes in
standard base64, and `encoding` is `base64`. `encoding` has no other value, and no other kind of
part carries it. A reader that does not know `encoding` still reads one string, and shows base64
text where the bytes were not text. `Part.Raw` in `pkg/sessiondata` applies the rule.

Every `unknown` part landed before `encoding` existed is one string with no `encoding`, and reads as
it always did. Where its bytes were not valid UTF-8, that string holds U+FFFD in their place.
Claude Code and the asz plugin write UTF-8, but such bytes can still reach an adapter. A write cut
short and joined to the next line is one way, and a workflow script saved in another encoding is
another. Measured on 2026-09-11 on one machine's storage root of 60 sessions, 151 of its 408,439
parts were `unknown`: 150 workflow scripts and one block of a type the dialect did not describe.
None of them held U+FFFD. So none had lost a byte, and all of them keep the form without
`encoding`.

Media is kept inline. A `media` part's `data` is the base64 text the runtime wrote, as one JSON
string. `media` is its media type, and `bytes` is the length of the base64 text. There is no
separate store for media. The measured corpus held 41 image blocks, so inline costs little. A
runtime that sends images routinely should be measured again.

A `result` can carry `text` and `data` together: the readable output, and a structured form of the
same result that the runtime gave beside it. A reader that reads only one of them loses the other.

`failed` is absent, not false, when the runtime said nothing. Most results carry no error flag,
and reading that as success would report something the runtime never claimed.

The set of kinds is small because the thing described is small. Measured across a corpus of 3,032
Claude Code files, six content-block shapes exist and four cover 99.99% of content blocks. The
Claude Code [Session Data mapping](../adapters/claude-code.md#session-data-mapping) gives the
counts.

## Dropped

```json
"dropped":[{"what":"reasoning signature","bytes":588,"why":"a provider verifies it; a reader cannot read it"}]
```

`dropped` exists so a loss is stated rather than silent. It lists only what the dialect understood
and chose not to carry. Everything it did not understand travels as an `unknown` part, so a later
version of the dialect can interpret it without collecting again.

The source bytes are not kept, so a `.sd` file is the only landed copy of what it holds, and
everything above the adapter treats it as the authority. A conversion error on content the dialect
understood, a message, a thought, a call or a result, can be corrected only by collecting again
from the runtime's source, and only while that source exists. Claude Code deletes its own
transcripts, and a storage root outlives them (see [Storage Root](storage-root.md#size)). Only an
`unknown` part can be read again from the store alone, because only it carries its original bytes.

## Producing Session Data

There is no Go interface to implement. An adapter is whatever lands Session Data in a storage root.
Nothing above it reads a runtime field, so another runtime needs no code on the assembling side
(see [What travels](storage-root.md#what-travels)).

An adapter writes:

- A header on every file, with `adapter`, its name and contract version, and `dialect`, beside the
  other fields a reader requires. See [Header](#header).
- Records with role-named identifiers and parts, one part for each piece of content, in source
  order. See [Addressing a record](#addressing-a-record).
- The closing line. The writer in `pkg/sessiondata` adds it on close.
- Files in the layout and under the names [Storage Root](storage-root.md) shows. Each is written
  once and made read-only, numbered one above the highest sequence already in the session, while
  the session's lock, `<session-id>/.lock`, is held.

The built-in adapters are wired into `cmd/asz` by name, so a third-party adapter is a separate
program that writes the storage root. `asz parse` finds sessions by listing the root, and `asz view`
serves the chains the root holds, so such a root is assembled and shown like any other. The index is
not part of the contract. `asz parse` builds a session's index from its landed files when the
session has none, and never reads past the index it has (see [The index](storage-root.md#the-index)).
So a program that adds files to a session that already has an index removes its `index/` directory.
That costs only a rebuild.

A dialect also has a [glossary](../concepts-and-designs/glossary.md#adapters), keyed by the
dialect: what its runtime calls each name the model uses. `cmd/asz` gives the page and
`asz glossary` the Claude Code glossary, whatever dialect a conversation was read in.
