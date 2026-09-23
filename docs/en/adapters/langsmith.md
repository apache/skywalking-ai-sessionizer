# LangChain

How an agent built on LangChain or LangGraph maps onto the
[unified conversation model](../concepts-and-designs/unified-conversation-model.md), what the
`langsmith-ingest` adapter reads, and what the wire cannot supply.

For turning it on, see [LangChain and LangGraph](../setup/langchain.md). For the words, see the
[LangChain glossary](langsmith-glossary.md).

## Where the data comes from

`langsmith`, the tracing client, is a dependency of `langchain-core`, so every LangChain
application already carries it. It is pointed at asz with environment variables and sends what the
graph did. asz accepts what that client sends; it carries none of its code and claims no
affiliation with the product it was written for.

Everything below was measured against a running agent, and the captures are in the repository under
`tests/apps/langchain/testdata`. Where a number appears here, that is where it came from.

## What the client does

| Measured | Result |
| --- | --- |
| Endpoint | `POST /runs/multipart`, preceded by `GET /info` every time |
| Compression | none, unless `/info` advertises it — so this receiver does not |
| Success | 202 |
| Unfinished runs | 3 of 18 arrivals in one capture carried no end time; those runs completed 6.0 s later |
| A rejected batch | retried with the same bytes, 10 ms later |
| Content | a 46,049-byte command and a 32,000-byte result arrived whole |

Two of those decide the design. Runs arrive more than once, so a run is a thing that is *updated*
rather than a thing that is *reported*. And a rejected batch comes back identical, so delivery is
at-least-once and the receiver deduplicates.

An update carries only what changed. `update_run(id, outputs=...)` sends the id and the outputs,
with the trace, the project, the kind of run and the dotted order all null — read on its own,
a run belonging to no conversation and of no kind. So the runs that have started and not ended
are remembered, and an update is completed from what its own start said. Nothing is invented:
every field comes from an earlier arrival about the same run.

## The mapping

| Model | LangChain |
| --- | --- |
| Conversation | the thread key in a run's metadata, with the project |
| Session | the same |
| ExecutionStream | `main`; a tool that ran a program is a `child`, a tool that made a model call is `auxiliary` |
| Run | `trace_id` — one invocation of the graph, which is one turn |
| `llm.call` | a run of type `llm`; the call is its id, shared by every arrival |
| `tool` | a run of type `tool`, joined to its call by `tool_call_id` |
| `message.external` | the human message inside the root run's `inputs`, or a traced function's arguments |
| Conversation name | the first question asked in it, or the name of the run that began it |
| `message.assistant` | the model's content in a run's `outputs` |

### A record is an arrival, not a run

A run is posted when it starts and patched when it ends, so the same run reaches asz more than
once. Each arrival is its own record, with the run as its `call`.

They cannot share one record id. The index keeps the first entry for an id, and in the streaming
capture a tool's result exists only in the patch — one id would have hidden it, and the
conversation would have shown a tool call that never returned.

**Only the arrival carrying the run's end carries its content.** Every text block of every fragment
becomes a message, so a post and a patch that both carried text would emit the assistant message
twice, in a round that called itself verified with nothing unresolved: a wrong conversation that
passes every check. An arrival with no end lands its bytes as data instead.

### A nested agent is its own stream

A tool whose implementation runs an agent produces model calls that continue nothing in the
caller's message list. The boundary is the tool run: anything below one is work that tool started
and lands in a stream of its own, while the tool run itself stays with its caller, because the call
and its result are the caller's evidence. That is the same shape a Claude Code sub-agent has.

A tool with nothing below it opens no stream; it is an ordinary step. A tool with only model calls
directly under it opened a stream for a plain call of its own, not for an agent: the stream is
`auxiliary`, the continuity check skips it, and the call stays a tool step. A tool with a chain of
any kind directly under it ran a program, and the stream is a `child` started by an `agent.call`.
Measured on the subagent capture, whose three tools are one nested graph, one single model call and
one leaf: one main stream, one child stream, one auxiliary stream, one agent call, and two tool steps.

A nested agent does a task for its caller and hands the result back, and the conversation records
that too. The tool run's result is what the caller received, and it is exact: the child stream is
the work under that one tool run. So the child's `agent.output` has a `result_of` relation to the
`agent.call` that started it, with the call's result as its evidence, and its `returned_value` is
observed. An auxiliary stream's model call leads back to its tool step the same way. Nothing here
arrives as a notification, as it does when Claude Code runs a child in the background, because a
LangChain tool returns only when the work under it has finished.

The session remembers which of its runs were tools, so a run arriving in a later request still
finds the tool it ran inside. That memory is derived and disposable — deleting it changes nothing
already landed, because a record's stream is the directory it is in.

The two halves of that join can arrive in either order. When a tool's own records landed before
anything was known to have run inside it, the link to the child stream is landed on its own,
once, in the caller's stream: the call comes from the tool's own result and the stream from the
runs that ran inside it. Without it the child is real and nothing names the call that started it,
which assembly reports as an orphan for ever.

A batch can arrive before the one carrying the runs it ran inside. A run is queued when it
**starts** — measured, in the slow-tool capture the tool and its node are posted with no end time
and completed by a patch two requests later — but the client drains that queue with several
sending threads at once, and this receiver advertises sixteen, so queue order is not arrival
order.

Placing such a run would be a guess, and the guess is always the parent lineage, which gives an
independent agent's model context and its answers to its caller. That is not a thin conversation
but a wrong one. So a request whose ancestry has not arrived waits: it is tried again at the end
of the same pass, because the batch that overtook it is very often already further down the same
inbox, and then for up to three more passes. After that it is landed with whatever ancestry did
arrive, because a trace that never completes must not hold its evidence out of the conversation
for ever.

### A graph's own runs land trimmed

Most runs are the graph's own: nodes, prompts, branches. In one capture they were 30 of 39 runs and
**74.4% of the bytes**, because each repeats content its model call already carries.

Landing them whole would nearly quadruple a conversation for content it already holds. Skipping
them would lose the shape that says why an agent looped. So the envelope and the `langgraph_` and
`ls_` metadata land as a data record, the repeated content does not, and the record says how many
bytes went with it.

| What is kept | Share of the request |
| --- | --- |
| everything | 74.4% |
| envelope and all metadata | 19.9% |
| envelope and graph metadata only | 15.4% |

The test is who made the run, not what the field holds. Testing the content does not work: the
repeats are not all message lists and they are not under one name — one capture held 164 KB of the
message list under `output`, and another 104 KB of a routing value that quoted a tool call. Every
test on the shape kept something large that was already landed elsewhere.

### A decorated function is not the framework

A `@traceable` function has no graph and may have no model call at all. Its arguments and its
return value live nowhere else, so for those runs the fields are kept — dropping them landed such
traces with no content in them at all.

The runtime says which is which on every arrival, and that is measured: every run of a decorated
trace carries `ls_method`, and no run of a graph trace does. A field that is a message list and
nothing else is still a repeat and is dropped; a field carrying anything beside it is kept whole,
because what is beside it is the function's own and is recorded nowhere else.

`ls_method` is inherited, so it is never read as proof on its own: decorating a function that
wraps a whole graph marks that graph's ordinary tools as traced too.

Such a function also calls its tools itself, so no model call names them, and a tool that names
no call is landed answering none. No call is made up for it. Making one up was tried three ways,
and each invented a second call for one real execution in some arrival order, because on this
wire a request never proves that no model call asked: the model call may simply not have arrived
yet. The tool's arguments are still landed from its run, so the step and its result are there;
what is missing is the model call that asked for it, when none did.

### What each call was sent

Every model call carries the whole conversation again in its `inputs`. Measured over twenty turns:
the first call's inputs were 210 bytes and the twenty-first's were 18,918, a factor of ninety, and
the thread was 1.63 MB on the wire.

A call's record keeps what the model said, so the conversation itself is **7 to 8% of what
arrived** — 126 KB of the 1.63 MB above. What the model was told is landed beside it, as a
provider body, the way Claude Code's bodies are: cut against what the session already holds by
`pkg/providerbody`, so a request shares its front with the request before it. A request is landed
from the first arrival of a run that carries inputs, a response from the arrival that carries the
run's end, and a repeat is a repeat. Measured, with every call's inputs and outputs:

| Capture | On the wire | Conversation | Bodies | Together |
| --- | --- | --- | --- | --- |
| long-conversation, 20 turns | 1.63 MB | 7.7% | 5.3% | 13.0% |
| large-content | 1.34 MB | 7.2% | 7.9% | 15.2% |
| subagent | 179 KB | 22.7% | 13.4% | 36.0% |
| three-turns | 145 KB | 19.7% | 13.5% | 33.3% |
| plain, one call | 20 KB | 31.5% | 16.1% | 47.6% |
| all nine captures | 3.75 MB | 10.1% | 7.7% | 17.8% |

Two things to read off that. Cutting pays on a long conversation, where each request is mostly
the one before: twenty turns of requests and responses, 217 KB on the wire, land as 87 KB. And it
costs on a short one, where the note that says how to rebuild a body is larger than what a small
body saves: three turns' bodies land at more than their size. Both are the price of the
continuity check having something to run on. `provider_bodies: false` on the adapter turns it off.

Both bodies of a call name the call, and `internal/assemble/provider.go` joins them by it. On this
wire a request knows its call — it is the run's own id — where a request Claude Code writes names
only the request before it and its prompt. So nothing about the order of a stream's calls is kept
or derived by the receiver. It was, and the assembler disagreed with it whenever a call's fragments
landed in an order the receiver had not seen: a session landed before the receiver kept anything, a
call whose first fragment landed in `main` before its ancestry arrived, a request held for a later
pass and then replayed. The run id is evidence; an order is an inference, and the assembler's is
the only one. A nested stream's records still name the tool they ran inside as their prompt, not
the trace, so every stream of one trace does not share the trace as its prompt.

On this wire a body is the framework's view of what it sent — the run's `inputs.messages`, in
LangChain's serialized message form — and not the bytes the provider received. The manifest says
which adapter it came from, and a reader must not take it for the wire.

### A conversation is named by what was asked

Nothing on this wire carries a name. The client names runs — `LangGraph`, `agent`,
`should_continue` — and those name the program rather than the conversation, so a list of them
would say the same thing about every one.

So the name is the first question of the conversation, trimmed to about a line. A conversation
that began without words — a decorated function called with arguments — takes the name of the run
that started it. Both are landed from the arrival that carried them; neither is invented.

It is landed once, because the fold takes the last name it finds, and naming every turn would
change a conversation's name under a reader each time it answered. The record carries no time: a
name did not happen at a moment, and giving it the collector's clock moved the conversation's last
moment to whenever it was collected.

### Ownership is a namespace

A thread key alone is not an identity: two applications can both use the project `production` and
the thread `123`. The adapter names the dimensions that own a conversation, defaulting to project
and thread.

A supplied key is never used as a path. The captures carry `../outside`, `team/customer`, `_hidden`
and `会话-1` as real thread keys, so the session directory is derived from them: a readable slug and
a digest of the whole namespace, under an `ls-` prefix that keeps it off the leading underscore
enumeration skips and away from every Windows device name.

A trace with no supplied key is not given one. It lands under a name that says the identity was not
supplied, and nothing merges it with anything else.

## What the wire cannot supply

- **A child's own conversation.** An agent invoked with its own `thread_id` reports the parent's on
  every one of its 30 runs: the surrounding run context wins and the child's key never reaches the
  wire. A sub-agent's work belongs to the conversation that delegated to it, and no configuration
  can change that.
- **Which call a failed tool answered.** A tool that raises has empty outputs and names the call
  nowhere. asz reads it from the model call that asked for the tool, and only when exactly one call
  of that name in that trace is unanswered. Otherwise the result lands joined to nothing.
- **A context reset.** LangChain has no compaction, so a conversation is one epoch however long it
  runs.
- **A delegation, by name.** Nothing on the wire says a nested run is an agent, and nothing names
  what it was asked to do. The boundary is read from the run tree instead: work that ran inside a
  tool is that tool's. Whether that work was a program or a single model call is read from what
  ran directly under the tool, and when the tool's own records land before anything has run under
  it, it is a child until the first nested run says otherwise.
- **Which call a tool answered, across requests.** A failed tool is joined to a call only when the
  request carrying it also carries the ancestry that says which agent it belongs to. Otherwise the
  result lands joined to nothing, because a candidate that merely looks unclaimed may belong to
  another agent entirely.
- **Which tools write.** A LangChain application names its own tools, and nothing on the wire says
  whether one changes the workspace or only reads it. The change recorder is told which to watch,
  by whoever runs it, because there is no convention to infer it from.
- **The provider's own request.** A run's `inputs.messages` is the framework's view of what it
  sent, not the bytes the provider received, and that is what lands as the call's request body.
  What the provider was actually sent, and what it cost, are not on this wire.
- **Which question is new.** An application that keeps its own history sends the whole
  conversation as a turn's input. What opened the turn is read as the run of messages at the end,
  after the last thing anyone else said. An application that reorders its history, or repeats a
  question verbatim, cannot be told apart from one asking again.

## What it supplies that a transcript does not

- **Work in progress.** Runs arrive before they finish, so a slow turn is visible while it runs and
  a process that died mid-turn still leaves what it had reported.
- **Per-call duration.** Every run carries its own start and end.
- **The whole message list per call.** `inputs.messages` is what the framework sent, so continuity
  between turns can be checked rather than assumed.
