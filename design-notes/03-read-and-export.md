# Design Plan 03 — Read and Export

**Status:** sections 0-4 are implemented. Section 5, the viewer, is designed and
not built.
**Scope:** getting a conversation out of the round chain and in front of a reader. Collection is
Plan 01 and assembly is Plan 02; both are implemented and neither changes.

The plan changed shape once already. It began as "export and preview" and the first real question -
what does a viewer read? - answered itself immediately: it reads the round chain, because a second format would
only drift from the first. The interesting question turned out to be the one behind it, in
[section 3](#3-the-contract-is-the-structure-file-and-that-exposes-a-hole): the model is
runtime-agnostic about structure and not about content, and a viewer is the first thing that would
notice. [Section 4](#4-two-formats-sf-and-sd) proposes the two formats that fix it.

Every number below was measured on the same corpus Plans 01 and 02 were measured on: 62 sessions,
1,100.1 MB of landed records, 319,264 assembled nodes. Where a number is missing it says
`unmeasured` rather than an estimate.

---

## 0. Why this plan exists

**Nothing has read the output yet.** Plan 02 produced 319,264 nodes across 62 sessions and no reader
has looked at one. That matters more than it sounds: most of the model's decisions were justified by
a claim about reading — a tool use is one step because *a reader thinks of it as one event*, a child
agent's output belongs to the child because otherwise *a rendered conversation shows the same work
twice*, a delegation stays inside one Talk because *a reader sees one interaction*. Those are
falsifiable claims about reading, and only a reader can falsify them.

So this plan's real output is not a viewer. It is the set of decisions a viewer forces.

### Three things a reader needs that the chain does not carry

**1. There is no time anywhere.** Measured: 0 of 319,264 nodes carry any temporal field.

```text
100%  id · kind · revision
 99%  parent · ref · stream
 64%  attrs
  0%  anything temporal
```

This is not an oversight. A round is deliberately time-free so that the same evidence reproduces
the same bytes and the same digest. But a node's *observed* time — when the runtime says the thing
happened — comes from a landed record that is immutable, so it is just as reproducible as the
node's parent or kind. It could be carried; it is not. A viewer cannot draw a timeline, group by
day, or say "twenty minutes later" without opening every referenced record.

**2. A Talk has no name.** A reader opening a session sees a list of ids. The largest session has
922 of them. Nothing in the chain distinguishes one from another.

**3. Nothing resolves a reference to bytes.** A node says `{"seq":1,"row":1004}`. The only thing
that turns that into text is `asz show`, which prints one record. There is no path from a folded
conversation to what anybody said.

### And one the maintainer raised

**A viewer's fold is not the assembler's fold.** The assembler applies every round in order and
keeps the result. A viewer may want the state as of an *earlier* round, or the *history* of one
entity across rounds, or to render rounds as they *arrive* rather than reading them all first. The
rounds hold enough to answer any of these. Which of them to serve, and how, is undecided.

---

## 1. Where the bytes are

The whole corpus, by what the bytes actually contain:

| MB | share | what |
| ---: | ---: | --- |
| 370.0 | 33.6% | scaffolding — record ids, usage blocks, envelope |
| 334.8 | 30.4% | tool response, content block |
| 146.4 | 13.3% | **thinking signatures** |
| 83.8 | 7.6% | tool response, **a structured duplicate of the same output** |
| 75.0 | 6.8% | tool command |
| 58.7 | 5.3% | injected context |
| 18.4 | 1.7% | message text |
| 14.3 | 1.3% | assistant text |
| 0.1 | 0.0% | **thinking text** |

Four readings of that table, each of which constrains the contract:

**Tool responses are 38.1% and are 6× the size of the commands.** Asking what a tool *did* is cheap;
carrying what it *returned* is the whole problem.

**What a person came to read is 3.0%.** Message text plus assistant text is 32.7 MB of 1,100 MB. The
conversation as read is a rounding error on the conversation as stored.

**13.3% is cryptographic signature on thinking blocks whose text is 0.1 MB.** A signature exists so
a provider can verify a block handed back to it. It means nothing to a reader, and it is the second
largest single category in the corpus.

**7.6% is the same tool output stored a second time.** A main-stream tool result carries both the
content block and a structured form with `stdout`/`stderr` split out. Both are real — the structured
one has detail the block lacks — but a reader wants one of them.

### The share is not stable, which rules out a fixed policy

Per session, over the 28 sessions larger than 1 MB:

```text
tool-response share    min 17.3%    median 35.1%    max 83.5%
```

A rule tuned to the median is wrong by a factor of two at both ends. Whatever the contract does
about tool output, it cannot be a constant.

### Compression is not the answer either

| | size | ratio |
| --- | ---: | ---: |
| largest session, landed | 200 MB | — |
| zip / gzip | 68 MB | 2.8× |
| zstd -19 | 45 MB | 4.3× |
| **its round chain** | **21 MB → 1.4 MB** | **15×** |

Rounds compress fifteen times because they are tens of thousands of near-identical structured lines.
Landed content does not, because a third of it is high-entropy signature and captured output.

Removing the two known-wasteful categories *before* compressing beats compressing harder: 45.2 MB →
26.9 MB, a 40% cut that no compression level recovers on its own.

---

## 2. What a reader asks for

A Talk is the natural unit — it is the thing a reader opens. Measured across all 4,941 of them,
structure only, no content:

```text
one talk's subtree     p50   12.7 KB    42 nodes
                       p90   43.4 KB   140 nodes
                       p99  111.1 KB   348 nodes
                       max  413.8 KB  1,475 nodes
```

The median talk's structure fits in a network round trip. The whole largest session is 16 MB of
structure — too much for a first screen, fine for a background load.

That asymmetry is the shape of the answer: **structure is small enough to be eager, content is not.**

### 2.1 Nothing needs to be cached

A design review set the threshold: if rendering a p99 talk takes longer than 200 ms, build an index
for it; otherwise never. Measured on the largest real session, 53,106 nodes and 922 talks:

| | |
| --- | ---: |
| fold the whole chain | 302 ms |
| build and order the talk list | 1 ms |
| **one talk's subtree, p99** | **13 µs** |
| walk every subtree in the session | 11 ms |

So: fold on demand, cache nothing. Four independent designs each proposed a derived read artifact -
a bundle, a view directory, four projections, a locator index - and all four were specified before
anyone had rendered a single talk.

The first measurement said p99 was 104 ms, which looked like it might justify one. It was
`View.Children` scanning all 53,106 nodes on every call, so a 42-node subtree cost 2.2 million
comparisons. A parent-to-children map built once took the same walk from 22 seconds to 11
milliseconds. The lesson is worth keeping: **measure the naive path before designing around it**,
because the first number may be measuring the implementation rather than the problem.

---

## 3. The contract is the structure file, and that exposes a hole

**A viewer reads the round chain. It does not read landed files, and it does not read a format
invented for viewers.** The chain already is the read contract: it carries the structure, it references the
content, and it is verifiable on its own. Inventing a second shape for viewers would mean two things
to keep in step, and the second would be the one that drifts.

But the chain references content; it does not describe it. And that is where the layering breaks.
[Section 4](#4-two-formats-sf-and-sd) is the answer that came out of it.

### 3.1 What is runtime-agnostic today, and what is not

The project's rule is that a runtime's vocabulary stops at its adapter. Measured against the code,
that is half true:

| | who interprets it | runtime-agnostic? |
| --- | --- | --- |
| **structure** | the adapter maps its identifiers onto index roles | **yes** |
| **content** | nobody — the payload is the source bytes, untouched | **no** |

Landing preserves payloads byte-for-byte, on purpose: it is what makes a landed record replayable
and its digest meaningful. The consequence is that the only description of what is *inside* a
payload is the runtime's own schema.

So a viewer rendering one `message.assistant` node has to do this:

```text
node.ref {seq:1, row:1004}
  -> landed record
    -> payload.message.content[0].text        <- Claude Code's shape, not the model's
```

Nothing stops it, and nothing tells it that walking `.message.content[]` is an adapter's business.
The first viewer that ships would hard-code one agent product's JSON into the reader, and the
model's whole claim to be runtime-agnostic would be true only of the parts nobody looks at.

**There is also no adapter interface at all.** `internal/adapters/claudecode` is a package of free
functions that `cmd/asz` calls by name. Nothing states what an adapter must provide, so a second one
has nothing to implement against.

### 3.2 Two axes, currently collapsed into one

A landed header carries a single `adapter` string, `claude-code-local/0.1.0`. It is doing two jobs:

- **transport** — how records arrive: reading local files, or receiving a push
- **dialect** — how records are interpreted: Claude Code's schema, Codex's, another agent's

Those vary independently. A push receiver for Claude Code shares every interpretation rule with the
local reader and no acquisition code. A local reader for Codex shares the acquisition shape and no
interpretation rules.

```text
                    dialect: claude-code    dialect: codex     dialect: …
transport: local    implemented             —                  —
transport: push     —                       —                  —
```

Keeping the *transport* in the adapter name is right and already documented: a push adapter is a
separate entry rather than a mode flag, because the collection posture changes what can be promised
about completeness. Keeping the *dialect* there too is what makes the table above impossible to
fill in without duplication.

### 3.3 The surface an adapter has to provide

Three responsibilities, of which only the first two exist and only one is named:

| | today | needed |
| --- | --- | --- |
| **acquire** — get records into the zone | `claudecode.Collector`, called directly | a transport interface; pull and push are two implementations |
| **interpret structure** — identifiers onto roles | `claudecode.IndexEntry`, a free function | a dialect interface, dispatched by the landed header |
| **interpret content** — a payload into something readable | *missing* | a dialect method returning a runtime-agnostic content unit |

The third is the one a viewer is blocked on. Its shape is constrained by things already measured:

- It must be able to return **part** of a payload. A single tool result runs to 1.1 MB, and the
  tool-response share of a session varies from 17.3% to 83.5%, so truncation cannot be a caller's
  afterthought.
- It must report **content state**, not just bytes. `available`, `truncated`, `redacted`,
  `unavailable` are already in the model; a reader has to be told which one it got, or the model's
  honesty rule stops at the last hop.
- It must be addressable by exactly what a node carries: a landed record and an optional block
  ordinal. Nothing else is in scope, because nothing else is in a round's reference.

An adapter that cannot interpret a payload it produced is still useful — the structure is already
assembled — so this is `unavailable` rather than an error, and a reader is told the content cannot
be described rather than shown a guess.

### 3.4 What this changes about the plan

The viewer is no longer the first thing to build, and it was never the point. **The adapter contract
is,** because a viewer written before it exists will encode Claude Code into the reader, and the
second agent product will find that out the expensive way.

It also reframes what Plan 03 is for. It is not "export and preview". It is the point at which the
model has to be honest about content the way it is already honest about structure.

---

## 4. Two formats: `.sf` and `.sd`

The names say what each is for. **Session Flow** is the structure — how the conversation went.
**Session Data** is the detail — what was actually in it.

```text
data/<session>/streams/main/
  transcript-<ts>-000001.jsonl     landed    the source bytes, byte-identical, 0444
  transcript-<ts>-000001.sd        derived   the same records, normalised

data/_conversations/<id>/
  rounds/r000001-<digest>.sf       published the structure, immutable, digest-chained
  conversation.state               cache     the head pointer
```

`.sf` is what was called `asb`. Nothing about it changed but the name and the schema string.

### 4.1 Why a second file rather than a read-time conversion

The alternative is to leave landed payloads as they are and have the dialect interpret them each
time something reads one. That was the shape of §3.3, and it is worse in three ways.

**The conversion is already happening.** Collection parses every payload to build the index. Doing
it again per read is the same work, an unbounded number of times.

**The reader cannot be trusted to stay clean.** A dialect method that returns content on demand is a
rule that has to be *obeyed*. A normalised file on disk is a rule that is *enforced* — there is no
runtime shape to reach for, because the reader never receives one.

**The dialect surface is bigger than it looks.** Measured: `toolUseResult` takes **49 distinct key
sets** across the corpus and is a bare string 7.2% of the time. Bash returns
`stdout`/`stderr`/`interrupted`, Edit returns `structuredPatch`, Workflow returns
`runId`/`taskId`/`transcriptDir`. That is a per-tool schema, and it is exactly what must not reach a
reader.

The cost of a second file is that it stores content twice. That is acceptable only because **`.sd`
is derived and disposable**, in the same sense the index is: raw payloads stay, so a wrong
normalisation is a rebuild, not data loss. Nothing may treat `.sd` as authoritative.

### 4.2 The vocabulary is small because the industry settled it

Every content block in 3,032 files, 1.1 GB:

| count | Claude Code block | keys |
| ---: | --- | --- |
| 108,243 | `tool_result` | `content` `is_error` `tool_use_id` |
| 108,149 | `tool_use` | `id` `name` `input` `caller` |
| 60,382 | `thinking` | `thinking` `signature` |
| 22,458 | `text` | `text` |
| 41 | `image` | `source{data, media_type}` |
| 1 | `fallback` | `from{model}` `to{model}` |

**Six types, four of which cover 99.99%.** This is the strongest evidence that a unified format is
worth defining: the thing being unified is not a moving target. A message, a thought, a call, its
result, and an attachment is the whole of what an agent does.

### 4.3 What a `.sd` record carries

One `.sd` per landed file, at the **same sequence number**. That is deliberate: an `.sf` node
already addresses content as `{seq, row, block}`, and keeping the sequence and the row makes the
existing coordinate address the normalised form unchanged. No second addressing scheme.

```jsonl
{"h":1,"schema":"sd/1","dialect":"claude-code/1","seq":1,"session":"…","stream":"main",
 "of":"transcript-20260901T003722Z-000001.jsonl","of_sha":"<digest of the landed file>"}
{"row":1004,"id":"<record id>","parent":"<its containment parent>","call":"<provider call>",
 "run":"<the agent loop>","ts":"2026-08-31T16:14:42.170Z","trigger":"external",
 "parts":[{"k":"text","text":"Two answers, and the second is the one that needs specifying…"}]}
{"row":1005,"id":"…","parts":[{"k":"call","id":"toolu_01…","name":"Bash",
 "input":{"command":"make check"}}]}
{"row":1006,"id":"…","parts":[{"k":"result","of":"toolu_01…","failed":false,
 "text":"ok  github.com/…","state":"truncated","bytes":1118873}]}
```

Two halves, and the split is the point:

- **record level** — the identifiers the structure is built from, in ROLE names. `id`, `parent`,
  `call`, `run`, `trigger`, `ts`. These are what `internal/index` already stores; `.sd` is where
  the adapter writes them down instead of the index deriving them from a runtime's fields.
- **part level** — what the content IS.

**`.sd` says what the bytes are. `.sf` says what role they play.** A `part` of kind `text` might be a
person's turn, an agent's answer, or an injected reminder — which of those it is comes from the
structure, and `.sd` does not guess.

### 4.4 Part kinds

| kind | is | carries |
| --- | --- | --- |
| `text` | readable text | `text` |
| `reasoning` | the model's own reasoning | `text` |
| `call` | a request to run something | `id` `name` `input` |
| `result` | what it returned | `of` `text` `data` `failed` |
| `media` | an image or document | `media_type` `data` or a reference |
| `unknown` | the dialect could not describe it | the raw location, `state:"unavailable"` |

Every part also carries `state` (`available` · `truncated` · `redacted` · `unavailable`) and
`bytes`, the size of the original. Those two are not decoration: one tool result reaches 1.1 MB and
the tool-response share of a session runs from 17.3% to 83.5%, so a reader is routinely shown part
of something and has to be told so.

`unknown` is what keeps the format honest. A dialect meeting a shape it does not recognise records
where the bytes are and says it could not describe them — the same rule the rest of the model
follows, rather than a guess or a silent drop. The attachment list this project just removed failed
exactly here: it dropped 635 records it did not recognise.

### 4.5 The mapping, Claude Code to `.sd`

Record level:

| Claude Code | `.sd` | note |
| --- | --- | --- |
| `uuid` | `id` | |
| `parentUuid` | `parent` | |
| `message.id` | `call` | never `requestId` — a fabricated record can reuse a real one |
| `promptId` | `run` | one loop the agent performed for one trigger |
| `timestamp` | `ts` | |
| `origin.kind`, else `attachment.origin.kind` | `trigger` | reading only the first loses 27.6% |
| `logicalParentUuid` | `continues` | the record a new context resumes from |
| `toolUseResult.toolUseId`, or a notification's tool id | `tool` | the tool use this record is about |
| `toolUseResult.agentId`, a journal `agentId`, a notification's task id | `child` | the child stream this record names |
| the `wf_…` directory, or `toolUseResult.runId` | `batch` | children started together; a join key, with nothing in the model for it |
| `isCompactSummary`, `subtype`, `isMeta`, … | flags | as the index already maps them |
| `requestId`, `agentId`, `sessionId`, `cwd`, `gitBranch`, `slug`, `version`, `userType` | — | not carried; identity is the file's, or unused |

Part level:

| Claude Code | `.sd` part | note |
| --- | --- | --- |
| `content[i].type == "text"` | `text` | |
| `"thinking"` → `.thinking` | `reasoning` | **`.signature` is dropped** — 146.4 MB, 13.3% of the corpus, and the reasoning text it accompanies is 0.1 MB. It is material a provider verifies, not something a person reads. |
| `"redacted_thinking"` | `reasoning`, `state:"redacted"` | |
| `"tool_use"` / `"server_tool_use"` | `call` | `caller` is `{"type":"direct"}` on all 108,149; carried as `unknown` if it ever is not |
| `"tool_result"` → `content`, `is_error` | `result` | absence of `is_error` does not mean success |
| `"image"` → `source{data, media_type}` | `media` | |
| `"fallback"` → `from`/`to` model | `unknown` | one occurrence; a control event, not content |
| `message.content` as a bare string | one `text` part | 4,429 records |
| `toolUseResult` object | merged into the `result` part | **49 key sets**; the readable output is `stdout`/`stderr`/`content`, the rest is per-tool structure that does not survive normalisation and is left in the landed record |
| `toolUseResult` string | `result.text` | 7.2%; would break a struct decode |
| `attachment.text` / `.content` | `text`, or `unknown` | 28 attachment types, heterogeneous |

**An MCP call is a `call`.** Measured: 39 of 26,940 tool uses, structurally identical to any other —
same block type, same keys, same `toolu_` id scheme, same result shape. The server is a prefix on
the name, `mcp__<server>__<tool>`, which is an attribute and not a kind. The sample is thin, 0.1%,
so this is stated as observed rather than as a rule.

### 4.6 What else changes

| | today | after |
| --- | --- | --- |
| `pkg/asb` | the round chain | **done** - `pkg/sessionflow`, schema `sf/1` |
| `*.asb.jsonl` | round file | **done** - `*.sf` |
| — | | `*.sd`, and a package that reads and writes it |
| `internal/index` | built from raw payloads by the adapter | built from `.sd`, dialect-free |
| the adapter | collect · index · (interpret content, missing) | **collect · convert to `.sd`.** Nothing else. |

That last row is the whole point. Once `.sd` exists, the adapter's only remaining job is turning its
runtime's bytes into the unified form. Everything above it — index, assembly, structure, rendering —
reads `.sd` and has no dialect in it at all. **A second adapter is one converter.**

Two things deliberately do NOT change:

- **Landed files stay, byte-identical.** They are what `.sd` is derived from, what `asz verify`
  checks, and what makes a wrong normalisation recoverable.
- **`.sf` stays immutable, time-free and digest-chained.** `.sd` is per landed file and therefore
  immutable by construction, so it needs no chain of its own.

### 4.7 Both vocabularies, side by side

The model strips runtime vocabulary, and that is right for storing a
conversation and wrong for showing one. A person debugging their own session
recognises `promptId`; telling them a "run" makes them translate in their head.
A person comparing two agent products needs the opposite.

So the adapter also declares a **glossary**: what its runtime calls each thing
the model names. A reader chooses which words to see, and neither is presented
as the other.

```text
asz conversation ID                 talk · run · llm.call · tool
asz conversation -terms native ID   talk · promptId · message.id · tool_use + tool_result
asz conversation -terms both ID     run  (promptId)
asz glossary                        the whole table
```

Measured on Claude Code: **59 terms, 36 named by the runtime and 23 derived
here.** That ratio is the useful part. If everything had a native name the model
would be a rename of one product rather than a model of any; if nothing did, it
would have no evidence under it.

Three rules keep the table honest, each enforced by a test:

- **Every name the model can emit has an entry**, even if only to say the
  runtime has no word for it. The failure otherwise is silent: a reader asks for
  native terms, meets a name with no entry, and cannot tell "no native name" from
  "nobody wrote one down".
- **An empty native name is an answer, not a gap.** A Talk, a Segment, a
  correlation quality - no agent product records them, and saying so beats a
  plausible word that sends a reader looking for a field that does not exist.
- **A native name must say where to find it.** Being told `logicalParentUuid`
  without being told it sits on a `compact_boundary` record is half an answer.

The glossary lives beside the extraction code, so a rename in the runtime
changes both together. It is keyed by dialect (`claude-code/1`), not by adapter,
because a push receiver for the same runtime would share every word of it.

### 4.8 Open

1. **Where the dialect is recorded.** `.sd` carries it in its own header, which settles it for
   reading. The landed envelope still conflates transport and dialect in one `adapter` string.
2. **Whether the index survives.** If `.sd` carries the record-level identifiers, the index becomes
   a lookup structure over `.sd` rather than a separate extraction. It may still earn its place as a
   fixed-width binary form; that is a performance question, and `unmeasured`.
3. **What `.sd` costs.** Dropping signatures and scaffolding should make it much smaller than the
   landed data, but the number is `unmeasured`. It decides whether `.sd` can simply replace landed
   files in an export.
4. **Media.** 41 images, base64 inline. Inline in `.sd` is fine at that volume and would not be at
   another; no store is proposed yet.

---

## 5. The viewer

The reference is the v0.8 preview page, and the direction it sets is right: an
**observability console**, not a chat log. A timeline across the top, a
transcript beside it, and an inspector that answers *where did this come from*.
That shape is what this model is for - a chat log would render the messages and
throw away every relation, quality and unresolved reference the last two phases
exist to produce.

### 5.1 What the preview establishes

| | |
| --- | --- |
| **Ground** | deep navy `#07111f`, surface `#0d1929`, lines `#263b55` |
| **Text** | `#e9f0f8`, muted `#9cb0c7`, faint `#6f859f` |
| **Accents** | blue `#58a8ff` · green `#35c980` · orange `#f4a641` · purple `#ac8cff` · teal `#31c7bd` · pink `#ef6eae` · red `#ff6f78` |
| **Type** | Inter, with a system mono for identifiers |
| **Layout** | toolbar · timeline canvas with a minimap and gap labels · transcript list · inspector |
| **Inspector** | four tabs: Details · Relations · Evidence · Raw |

Two details in it matter more than the styling.

**It already shows qualification.** `unavailable` appears six times and
`strong_inference` four. The preview was drawn before any of this was measured,
and it was drawn honestly - a viewer that hides how a join was made would undo
the phase that produced it.

**It has a gap label on the timeline.** Which is the segment question rendered:
a quiet period is drawn as a gap rather than silently closing one thing and
opening another.

### 5.2 What has changed under it

The preview names things the model has since renamed, and asks for two it cannot
have.

| preview | now | note |
| --- | --- | --- |
| `agent_response` | `message.assistant` | |
| `agent_instruction` | `message.external` | |
| `tool_activity` | `tool` | |
| `agent_call` · `agent.prompt` | `agent.call` | |
| `agent.run` · `agent_run` | `run` | |
| `control_trigger` | `trigger` on a Run | external or notification |
| **`tool_execution`** | — | **does not exist.** The preview draws a tool as call → execution → result. Only two of those are recorded. Three tools report a real duration; for the rest it is `unavailable`, and a timestamp gap is not a substitute. |

And three things it wanted are now available that were not when it was drawn:

- **a title**, in 41 of 42 conversations
- **a label on a child stream** - what that agent was asked to do
- **usage per provider call**, and **`turn.duration`** measured by the runtime on
  465 turns

### 5.3 How it reads

**Fold on demand. Cache nothing.** Measured on the largest real conversation:

```text
fold the whole chain           302 ms
build and order the talk list    1 ms
one talk's subtree, p99         13 µs
```

A design review set the threshold at 200 ms for building an index; the answer is
thirteen microseconds. Every derived read artefact that four independent designs
proposed - a bundle, a view directory, four projections, a locator - solves a
problem that does not exist at this size.

**Time comes from the index, not from the round.** A round is deliberately
time-free so its bytes stay reproducible, and a timeline needs a time per node.
Both hold: a node carries `{seq, row}`, the index carries the observed time for
that position, and the viewer builds the lookup once when it loads a
conversation. Nothing is added to a published round.

### 5.4 The shape

`asz view` - a local HTTP server on the standard library, no build step, no
dependency, no database. It serves one page and a small JSON surface over the
chain it already has.

```text
GET /conversations                     id, title, talks, span, size
GET /c/{id}                            the folded view: talks, streams, segments
GET /c/{id}/talk/{n}                   one talk's subtree, with times
GET /c/{id}/record/{seq}/{row}         one landed record, its parts
```

Four endpoints because the preview has four questions: which conversation, what
happened in it, what happened in this turn, and what exactly did this step say.

The inspector's four tabs come free from what is already stored:

| tab | source |
| --- | --- |
| Details | the node's `kind`, `attrs`, and its resolved time |
| Relations | every edge touching it, with `quality` and `via` |
| Evidence | its `ref` and `refs`, and what they resolve to |
| Raw | the `.sd` record: parts, flags, usage, and what was dropped |

### 5.5 Order of work

1. **The server and the transcript.** One conversation, its talks, one talk
   rendered with every step, quality badge and unresolved row visible. This is
   the slice that falsifies the most: whether a Talk is the unit a reader opens,
   whether quoting names one, and whether a wall of badges is readable.
2. **The timeline.** Needs the time lookup and the gap labels, and answers
   whether segments as measured look right to a person.
3. **The inspector.** Relations and Evidence are the phases 1 and 2 work made
   visible; Raw is the honesty check - a reader can always see what we stored.
4. **Export.** Only after a viewer exists, because until then nobody knows what
   an export has to contain.

### 5.6 What it must not do

- **No chat-log rendering.** Dropping relations and quality would waste the model.
- **No inferred timing.** A tool with no reported duration shows none.
- **No hidden unresolved references.** They are the product of the honesty rule
  and belong on screen, not in a log.
- **No cache before it is measured to be needed.** The numbers above say it is not.

## 6. Open questions

1. **Retention across two directories.** Carried over from Plan 02 and now sharper: a round
   references landed records, so pruning landed data while keeping rounds leaves a structure whose
   content cannot be read. A reader meeting that must be told the content is gone, not shown an
   empty one — which means content state has to reach the screen.
2. **Reading a conversation that is still being written.** `unmeasured`. Collection and parse both
   run while someone could be reading.
3. **Search.** Nothing indexes text today; the derived index holds identifiers only, deliberately.
4. **Where the dialect is recorded.** A landed header carries one `adapter` string that names both
   transport and dialect. Interpretation needs the dialect alone. Deriving it from the name by
   convention works for the one adapter that exists and will not survive the second. Adding a field
   changes an envelope that is already on disk in immutable files, so the migration has to be
   thought through rather than assumed: the envelope is versioned (`h:1`), and landed files are
   never rewritten, so a reader would have to handle both. `unmeasured`: whether any existing
   landed data would need a compatibility path, or whether re-collection is acceptable.
5. **Whether a runtime's own schema changes under us.** Claude Code's record shape is not a
   published contract and has already shifted during this project. Structure survives that - the
   adapter absorbs a rename - but content interpretation is a second surface with the same
   exposure, and nothing detects a shape change today. A dialect that meets a payload it does not
   recognise must report `unavailable` rather than guess, which is the same rule the rest of the
   model follows.
