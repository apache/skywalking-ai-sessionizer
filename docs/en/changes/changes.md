# Changes in 0.6.0

> In development, not yet released. `tools/release/release.sh prepare 0.6.0` removes this note.

## Kiro

- A new adapter, `kiro-local`, collects the sessions Kiro's V3 agent engine writes under `~/.kiro`:
  kiro-cli started with `--v3` or with `chat.agentEngine` set to `v3`, and Kiro Crew on its `kas`
  backend. It is on by default and needs nothing from Kiro. Measured with kiro-cli 2.28.0 and its
  engine `@kiro/agent` 0.66.26, and Kiro Crew 0.7.2, over 86 sessions and 3,101 records, and with
  Kiro Crew 0.8.0, which bundles kiro-cli 2.27.1 and engine 0.66.22, over 13 sessions and 2,069
  records.
  A session is read when its header names schema `1.0.0`; kiro-cli's V2 sessions and V3 sessions of
  another schema are counted, in `asz sources` and once in `asz collect`, and not collected. See
  [Kiro](../setup/kiro.md) and the [Kiro adapter](../adapters/kiro.md).
- A prompt carries no turn id in Kiro, nor does a message sent while a turn runs. Each belongs to the
  first turn named after it in the file, and waits to land until that turn is named, since a landed
  record is never written again. A prompt whose turn never starts lands with no turn once its file
  has been still for five minutes.
- A Kiro record's id is the engine's id followed by the record's digest. The engine gives one id to
  an approval it asks twice, 46 ids in the measured records, and assembly would have removed the
  second as a copy.
- Kiro's subagents are joined by the child's id, which the parent's start and end records and the
  child's own file share. A compaction is a context reset with its summary. A turn's `elapsedTime`
  is its measured duration. Kiro Crew's system prompt arrives as the first prompt of a session and
  is not named `system_prompt`, because nothing in the record tells it from a person's prompt.
- Kiro records no token counts, so no token metric is derived for it. Pushed, its sessions are the
  service `Kiro`.

## LangChain and LangGraph

- A run whose conversation cannot be told because what would say it is gone is not collected. It
  landed under a name saying its identity was not supplied, which put one conversation's evidence
  in another. Two cases do this, and each waits first, as a run waits for its ancestry:
  - an update that names no conversation of its own, whose start was forgotten, after a day or
    past 50,000 remembered runs, or never came;
  - a run that names no thread under a run above it that is not known: one that never came, was
    forgotten, or is known only from updates that named no conversation of its own. It used to
    take a thread from further up past such a run in the last case, although that run's start may
    have named another. An update that named its run's whole conversation still gives it to the
    runs below.

  A run remembered only from its updates no longer proves that it named no thread. A thread named
  before its project is known no longer lands with an empty project. An update that carries its
  conversation's whole key still lands with no start seen, as every captured update from the Python
  client does, and a trace whose runs are all known and name no thread still lands unassigned. Each
  pass that drops an arrival says how many, as `runs_not_collected`. The receiver's memory of runs
  moves to version 2, which records whether a run's start was seen, so every run it remembered is
  forgotten once.
- Two records that could carry a system prompt unnamed are named. An unfinished model call whose
  client put the prompt among its parameters, in the `extra` that lands whole with the envelope, is
  named `system_prompt` by that shape, as its tools already were. A graph's first input whose
  person's message carries, as its content, an object that takes a request's shape is named by that
  shape, as the LangChain page says a graph's first input is. The test of a root function's first
  input checks that record's own flags, where it checked every record's together.
- A LangChain JS or LangGraph JS application lands as a Python one does. Its client, LangSmith JS,
  writes a run's end time as the milliseconds since the epoch, so every run it sent was refused
  and nothing landed; the receiver now reads either time. It serializes every message as a
  constructor whose kind is named only by the class that ends its `id`, with its fields under
  `kwargs`, so a LangGraph JS conversation had no question opening it, a tool's result landed as the
  serialized message, and a system message in a decorated function's arguments landed unnamed; each
  of these now reads the message by its class. A decorated function carries no `ls_method` from it,
  and is known by the library that made the run, which both clients write. A model call inside
  a decorated function supplies no thread, because LangSmith JS passes the function's metadata to
  none of the runs inside it, and takes the function's, as the next entry says. Measured with
  LangSmith JS 0.10.8 on a LangGraph JS agent and a traced function, the two applications under
  `tests/apps/langchain-js`.
  Their captures sit beside the Python ones under `tests/apps/langchain/testdata`, and the recorder
  reads the chunked body that client sends.
- A run that supplies no thread takes the conversation of the nearest run it ran inside whose
  conversation was supplied. Its dotted order names the runs it ran inside, and the thread was
  supplied there. A model call inside a LangSmith JS `traceable` supplies none, so it landed in
  a conversation of its own under its trace, with its answer, while the question's conversation had
  no answer. A run that supplies its own key keeps it, and a trace that supplied none anywhere still
  lands unassigned. The conversation is decided once the run's ancestry has arrived, so the order
  runs arrive in does not change it. A run above that is not known stops the search, and the run
  waits for it, as the first entry here says. A run's own key decides from the first of its arrivals that brings one before
  the run lands, and once it has landed its later arrivals follow it, while the receiver remembers
  it. A thread an update names before the run's project is known does not decide while the run
  waits, and the run neither takes a thread from above in its place nor passes one below.
- A summary `SummarizationMiddleware` wrote now resets the context, as a Claude Code compaction
  does. The middleware marks its summary message with `additional_kwargs.lc_source` set to
  `summarization`. A model call sent that message starts a new epoch in its stream, with an
  `epoch.boundary` and the message as its `epoch.summary`, joined by `summarizes`. The summariser's
  own call stays in the epoch whose history it read. A summary is sent again with every later
  call, and the conversation keeps one reset for it. Nothing is inferred from how the message list
  changed: `trim_messages`, langmem and `RemoveMessage` write no mark and make no reset. A
  conversation landed before this keeps one epoch; the reset is taken when a request lands.
- A link to a plain model call inside a tool landed with its `auxiliary` flag twice. A request is
  placed once to check it can land and again to land it, and the second placement built on the
  first. Placement now leaves the request as it arrived.
- The page shows what a LangChain model call was sent. The Prompt tab read a LangChain request as
  Claude Code's shape and drew it as one empty message. It now shows each message in LangChain's
  own words, `system`, `human`, `ai` and `tool`, with the tool calls an `ai` message made and each
  tool result under the call it answers. A response shows the answer, the model and the token
  counts. What it added stays unavailable for a LangChain call, because no request names the one
  before it, and it now says so. It used to say the request before was not among the loaded bodies,
  here and for the first call of any chain, where nothing is missing. The renderer is Horizon's,
  pinned at `88e0110` for this, which also draws a light theme's scrollbars and other native
  controls light.
- A landed LangChain file is named for the collected time in its header. The collector read the
  clock once per pass for the name and once per file for the header, so a server that stores files by
  session and sequence and names them from the header, as the OAP does, named them apart from asz. On
  a real root, all 50 LangChain files were named apart from their header, by up to 99 milliseconds,
  and none of the 7,185 Claude Code files was. Files landed before this keep their names, because
  landed files are never rewritten.
- On a LangChain conversation, the system message lands in the provider bodies, which a reader that
  withholds anything is never served. The tools, sent in an out-of-band `extra`, land nowhere. Two
  other records can hold the request: the whole run envelope a model call lands when it arrives
  before it has finished, and the first input of a trace whose root is a model call with no message
  list. Each is named `system_prompt` when it holds inputs of any shape, and `tool_schemas` when it
  offers tools or functions, nested in the client's own configuration too. A decorated function's
  own arguments and result, and a graph's first input, are the application's own, so they are named
  only by their shape: a message of the `system` or `developer` role, a value under a key a provider
  takes the prompt by, or a list of tools. The shapes are listed on the LangChain page. On the
  captured corpus the client sends both out of band, and all 51 trace roots are chain runs, so the
  one record named is the arguments of the LangChain JS traced function, which hold its system
  message.

- A model call's record says whether LangChain made it for its own purpose, such as
  `SummarizationMiddleware`'s summary, from `lc_internal_call` in its start's metadata: it is flagged
  `internal_call`, and any other model call `turn_call`. Only a start decides, and an update carries
  its start's class, or none when the receiver no longer holds it.
- A request's manifest lists its messages in the form `langchain/1`, one digest each, with nothing
  left out of a message, and a response's the message it produced. On the 20 capture directories,
  every one of 80 message ids sent more than once has one form. A new capture, `streaming`, streams a
  graph and a loop over `model.stream()`; the loop sends back the chunks it joined, as
  `AIMessageChunk`, and a graph an `AIMessage`. See [LangChain](../adapters/langsmith.md).
- The order of a run's arrivals in one request no longer changes what lands. An update written
  before its run's start was completed only from the arrivals before it, so it had no kind and
  landed as a graph's own run: a model call's answer was dropped, and its response body was not
  landed. A run's arrivals in a request are now read together, so a model call's end is known when
  a failed tool is matched to the call that asked for it, and a response body lands after its run's
  request, so its reply is read against that request. Both clients write the start first. A client
  written by hand need not.
- `PATCH /runs/{id}` with a body that carries no `id` is accepted, and the run is the one the path
  names. LangSmith JS 0.10.8 sends its updates so when it does not batch, as with
  `autoBatchTracing: false`, and every one was refused, so none of the runs it traced finished. A
  body whose `id` names another run than its path is refused. `POST /runs` and `PATCH /runs/{id}`
  are now read the same way by the receiver and the collector, so a body the collector cannot read
  is refused when it arrives, where it was accepted and then set aside.
- A chat model invoked on its own, at the root of its trace, opens its conversation with the
  person's question. LangChain's tracer writes a chat model's messages as a list per prompt, which
  was read as one flat list with no person's message in it, so nothing opened the conversation and,
  with `provider_bodies: false`, the question was not landed at all.
- A trace that keeps making runs no longer remembers every run it made. Each arrival kept the
  whole trace's memory fresh, so a trace that ran for days kept every run past the day and the
  50,000 runs that bound what the receiver remembers, and every request saved them all again. A
  trace now forgets a run when the run is forgotten, and keeps the runs a remembered run ran inside,
  so a run arriving in a long trace still lands at once.
- The LangChain plugin files a tool's changes under the project the tool's run is traced under. It
  read the project from the environment alone, so with a project set in code, such as
  `LangChainTracer(project_name=...)`, the changes went to a session the conversation was not in.
  It now watches LangChain's tracer start each tool run and reads the project from the run, and
  makes the scan before the tool runs once both the tracer and the plugin have been told the tool
  started, which LangChain does before the tool runs, in either order. A tool call no LangChain
  tracer traces, or one traced with tracing turned off, records nothing, since no conversation holds
  its run. If the tracer cannot be watched, the plugin logs a warning and files changes under the
  environment's project, as before.
  See [the LangChain plugin](../setup/langchain-plugin.md).

## Claude Code

- A headless run's question is on the page. `claude -p` and an Agent SDK application write their
  prompt with `promptSource: "sdk"` and no `origin`, and only `origin.kind: "human"` was read as a
  person's input, so the page showed each answer without the question it answered. Such a prompt
  is now external input that opens its talk. A session collected before this keeps the gap, because
  landed records are never rewritten.

- What the runtime sent the model is named. In Claude Code 2.1.259 to 2.1.286, the versions
  measured, and in a runtime built on the Claude Agent SDK, the transcript holds the system prompt
  and the tool schemas the runtime sent, in a `prompt_snapshot` attachment, and the schemas of the
  tools it offers on demand, in a `deferred_tools_record`. asz landed both as injection steps with
  nothing saying so. A snapshot now carries the `system_prompt` flag, and either attachment carries
  `tool_schemas` when it holds tools, by its type and keys, never by the text. The rule is on the
  [Claude Code page](../adapters/claude-code.md#step-mapping). An operator who serves a
  conversation to the people the agent served can then withhold them by rule. Such a step never
  names a talk. The page shows the flags on the step. A session collected before this keeps its
  records as they are, because landed records are never rewritten. Collecting it into a new root
  names them.

- What each model call was sent is recorded as a list, so a conversation can tell where a call's
  history changed without reading a body. A provider body's manifest lists the messages its request
  held, one digest each, the system prompt without its billing header, the message its response
  produced, and the provider thread the request starts or continues, in the form `claude-code/1`
  ([Session Data](../formats/session-data.md#history)). Claude Code 2.1.302 continues a thread the
  provider keeps: a process's first request holds the whole message list, and each later one only
  the new messages and the id of the response it goes on from. Measured on one session of nine
  resumed turns, 14 requests: with each such history built from the call it names, every call's
  history was the one before it with the reply next, through six resumes, until the compaction. A
  redacted copy of that session is a test. See
  [Claude Code Provider Bodies](../adapters/claude-code-provider-bodies.md). A body landed before
  this carries no list.

## MCP calls

- A call to an MCP server names its server and its tool. Claude Code calls an MCP tool
  `mcp__<server>__<tool>`. A landed call part now carries `server` and `server_tool` when the name
  splits exactly, with one `__` after `mcp__`, and the step's `attrs` carry `mcp_server` and
  `mcp_tool`. A name with a second `__` is left whole, since the separator cannot be told apart
  from a name that holds it, and so is one whose underscores run together: `mcp__foo___bar` is
  `foo` and `_bar`, or `foo_` and `bar`. It used to be split as the first. A session collected before this has neither, because landed records
  are never rewritten.
- The Claude Code plugin records each call to an MCP server. Its hooks after every `mcp__` call
  write an `execution/1` record: the server and where its configuration came from, how the call
  ended, the time Claude Code measured around it, and the size and SHA-256 of the arguments and of
  the answer, never their text. The `changes` adapter lands them as a new kind, `execution`.
  Assembly does not read them. `asz.view` lists them as `tool_executions`, and a step names its
  records in `executions`, joined by the tool-use id. `mcp.enabled: false` in the plugin's
  settings turns them off. The shapes were measured on Claude Code 2.1.282, and the real hook
  payloads are the plugin's test data.
- `asz view` draws what the plugin recorded of each call to an MCP server. A call named by its
  server and tool sits on its own MCP lane, its card shows how it ended and the time measured
  around it, and the inspector gains an Execution tab. The Evidence tab lists each change and
  execution record after the step's own positions, and shows what the document carries for the
  picked one: the step's text, a call's result on its result position, or the record itself. It
  used to show the step's text under every position. The renderer is Horizon's, pinned at
  `c903e77` for this.

## The page

- A withheld step that names one empty part keeps its size of zero. It took the size of all the
  record's parts, which the asz.view page reserves for a step that names no single part.
- The page refuses an address holding `;`, where it dropped a `hide` written after one and asked for
  the whole conversation. The API refuses such a query, and now the page does before asking.
- The asz.view page says that `hide` ignores spaces around a name and empty entries, that a run
  journal's record is what names no stream, and that the withheld count is as of the last load.
- A view can withhold the system prompt and the tool schemas. An operator who serves a conversation
  to the people an agent served may need to keep from them what the runtime sent the model, while
  the operator still sees it. asz knows nothing about who is reading. It withholds by the flags the
  adapter set, never by the text or the size of a part. `view.hide` in the configuration lists the
  flags that `asz view` and `asz server` withhold, and by default it is the whole answer: a request
  that carries a `hide` parameter is refused with status 400. With `view.hide_override` on, a `hide`
  parameter on the document, record and files endpoints replaces `view.hide` for that request, and
  `hide=` with no name withholds nothing. A host that serves the API through its own route sets it
  on every request, and so decides per reader. The page passes a `hide` in its own address on to its
  document, record and files calls.
- A withheld step keeps its node, its flags and its size, loses its text and says `omitted`.
  `summary.withheld` counts what was withheld, and lists every name asked for, a zero included. So
  a filter over a root landed before the flags shows that it found nothing. For a reader that
  withholds anything, no call lists a provider body, and neither the files nor the record endpoint
  serves one. A request carries the system prompt and the tool schemas again, and a body is served
  whole or not at all. The rules are on the asz.view page, so a server that mirrors the format
  withholds the same way.
- The `hide` parameter spelled another way, any key whose letters alone spell `hide` in any case,
  such as `Hide`, `hide[]` or `hide[0]`, and a query that does not parse, are refused with status
  400, as a name that is not a flag a reader may withhold is. Either would otherwise read as no
  `hide` at all, and the reader would be shown everything.
- The files endpoint serves provider body files only. It served any landed file whole by its
  sequence, transcripts included, and the Prompt tab reads provider body files only. Any other
  file, and one whose header does not read, is refused with status 400.
- The page's copy buttons work when the page is served over plain HTTP from an address other than
  localhost. A browser gives a page the Clipboard API only on HTTPS or localhost, and without it
  every copy button did nothing and said nothing. Each copy button of the Prompt tab now sits in the
  title of what it copies, so the whole body and the settings copy while their section is closed.
  The renderer is Horizon's, now pinned at `e55f329` (apache/skywalking-horizon-ui#190).
- The page draws every shape asz lands for a record cut short or removed. A change record whose
  file list a budget cut short says how many files changed, and that their names were not kept,
  where it said no file changed. The Changes tab and the panel count its files, and a count over
  several records that cannot be exact is shown as at least that many. A removed execution record
  shows its time and outcome, where it said they were not reported. A provider body whose manifest
  has no segment list, which a third-party adapter may write, is read as having none, where the
  Prompt tab dropped the body. The renderer is Horizon's, now pinned at `1401336`
  (apache/skywalking-horizon-ui#198).
- The page draws where a model call's history changed: a `history.change` card just before the
  call, dashed and in a colour no other kind uses, with how many messages were kept, changed,
  dropped and added, whether their order or the system prompt changed, and what became of the
  earlier call's reply. Clicking it selects the call. The Prompt tab's second mode, now "What
  changed", says what the comparison found instead of comparing two bodies by position. A Claude
  Code 2.1.302 request that continues a provider thread holds only its new messages, and the old
  mode reported every such request as a rewritten history, or as the context replaced by a
  summary. The mode marks each message kept, changed or added where the pairing is certain, lists
  the messages dropped, and says a reset only where the conversation records one. On a root landed
  before histories were recorded, it says there is nothing to compare. The renderer is Horizon's,
  now pinned at `d5830a1` (apache/skywalking-horizon-ui#202).

## Collection

- A file that fails to land leaves nothing behind. Its records entered the index while it was being
  written, and it kept the number it took, so a failed rename left index entries for records that
  never landed, and a gap in the landed numbers that every later round reported as a missing file.
  Claude Code's transcripts and workspace changes and Kiro's sessions did both. Provider bodies
  stopped the session's pass at the failed file, before anything was saved, and they land in the same
  order now. Records enter the index only once their file has landed, and a file that did not land
  gives its number back.
- A landed file fits `max_delta_bytes`, unless it holds a single record that is still larger with
  every payload taken out. A collector cut a file by the bytes it read, and a record is larger than
  its source line by its envelope: 4 KiB of short lines landed as 19 KB. Files are now cut by what
  their records take once written. A single record larger than the budget lands alone, and its
  largest payloads are taken out, one at a time, until the file fits or nothing is left to take
  out. Its identifiers, its label and the list of its parts are never taken out, so a record with a
  label of 10,000 bytes lands as a file of 10,434 bytes under a budget of 4 KiB. A part taken
  out keeps its place, its kind and its identifiers, and says `too_large`, with the size of the
  original and the SHA-256 of what it carried in the new part field `sha256`. A part loses its data
  before its text, so a result whose runtime record of a written file is large keeps its readable
  text, and says `truncated`. A change record or an
  execution record is never taken out whole, since a step joins to it: a change record is made
  smaller in its own format, its largest files losing their hunks and then its file list going, so
  it keeps its identity and says `truncated`, as its entry in the document now says with `state`. A pasted screenshot measured 4.6 MB in
  one transcript line, and a file that size is more than a receiver may have to store at once. A
  provider body is fitted by its cutter, which never takes out the manifest: a later body names a
  piece taken out as it names any piece, a body of short strings with nothing to take out lands
  whole and is taken out whole, and such a body cannot be rebuilt from the root. In the document, a
  step that stands on a whole record says `too_large` when a part of it was taken out. `asz verify`
  counts both apart from damage, and still holds a body's record to its manifest. See
  [Too large to land](../formats/session-data.md#too-large-to-land).
- One command writes to a storage root at a time. `asz collect` and `asz server` hold the new root
  lock, `_root/.lock`, for each pass. `asz parse` and `asz push` hold it for their run, `asz
  scenario build` for each session it builds, and `asz scenario check` for each root it builds.
  `asz repack` holds the root it reads and the root it writes, and refuses a destination inside
  the root it reads, which every command would take for a session. Another command waits for it, and a
  watching pipeline does nothing that period and says so once. `asz view` and `asz index` only
  read and take none. The LangSmith receiver is the one writer outside a pass: it only adds
  requests to `_langsmith/`, and a pass takes them under that directory's own lock.
- A session goes to one owner: the endpoint, the configured `service_name` and the
  `service.instance.id` it was first sent under. `push.state` records it, as an `owner` line,
  before the first request carrying anything of the session goes out, so a request whose answer
  never came still leaves the session with its owner. After the endpoint, the service name or the
  instance changes, a session sent before is held. None of its files, rounds or metrics are sent,
  every pass says why, and `asz push` exits non-zero. A receiver holds a session whole, so its later
  files must not go somewhere else.
- A failed directory sync is reported. After the rename that publishes a landed file, a round or a
  state file, the directory is synced, an attempt to make the name survive a power loss as well, and
  a failure there was ignored without a word. It is now reported once for the directory, since a power loss may lose the
  newest names there; the write still succeeds, since the file is in place, which is what a process
  crash needs, and another process may have read it. A filesystem that cannot sync a directory at
  all, as some network, FUSE and container mounts cannot, reports nothing; on macOS a plain fsync is
  tried before a directory is called so, and on Windows none is synced.
- A source line larger than the budget lands in a file of its own. The Claude Code, Kiro and
  workspace change collectors read on to the end of the source to find where the long line ended,
  and landed every line up to the last one in the same file: one 4.6 MB line made an 8 MB file. The
  window now grows until it holds the line's end, and stops growing when the source was cut short
  while it was read.
- No landed line nests deeper than 256 objects and lists, which a reader may refuse and the OAP
  does, dropping every later record of the file. A part whose data nests deeper than its 253 levels
  of the line is taken out as a payload too large to carry is, whatever its size: `too_large`, or
  `truncated` while its text is left, with its size and SHA-256 kept. A change or execution record
  is never taken out whole: it is written again in its own format, without the field its format
  does not name, and says `truncated`. A provider body's piece, which is a whole tool or a long
  string, is taken out the same way. asz used to land data up to Go's own limit of about 10,000
  levels.
- The index takes a label or an identifier of any length, and a record of any number of parts. A
  string of 64 KiB or more, such as a long title, stopped the session's index from being written,
  so the session never parsed again; and the 65,537th part of a record took the first part's
  position, so its step replaced the first one's. The index schema moved to 16, and an older index
  is built again from the landed files.
- The index reads what a provider body records of its call's history, and the class of a model call.
  Its schema moved to 17, and an older index is built again from the landed files.
- The OTLP exporter over HTTP keeps its own connections. It shared the process's default HTTP
  transport, so closing it closed every other HTTP client's idle connections, and anything else that
  closed them, such as a test receiver shutting down, could break a request the exporter had just
  started: `transport connection broken: http: CloseIdleConnections called`, seen in CI with
  scenarios pushing in parallel.
- No landed line holds a byte that is not valid UTF-8. A JSON decoder accepts such a byte inside a
  string, so a tool input or other source JSON could bring one, and it landed as it was. A protocol
  buffer string must be valid UTF-8, so the OTLP export could never send that file, over HTTP or
  gRPC, and every request it was in failed on every pass. Such data is now taken out: `omitted`, or
  `truncated` while its text is left, with its size and SHA-256 kept. A change or execution record
  is written again in its own format, with U+FFFD in place of the byte. Either way the record's
  `dropped` says which part lost what and why, so it is not taken for a payload too large to land.
  See [Reading a record](../formats/session-data.md#reading-a-record).
- One file the OTLP export cannot send no longer holds back the others. It went in a request with
  them, the request failed, and none of them was sent; and the file being batched when that request
  failed was left out of the pass, under the request's error. A file that cannot be encoded is now
  reported on its own, under its name, on every pass, and the files beside it go. A failed request
  is reported as its own error, and the file being batched goes in the next request.
- The OTLP exporter over HTTP takes a 2xx answer whose body does not arrive whole as an error, such
  as one whose connection closes before the length it promised, and one longer than the 1 MiB it
  reads of an answer. It read the first as an empty answer, a full success, and the second as its
  first MiB, and recorded every file of the request as sent, though nothing said the receiver took
  them. They now go again on the next pass.
- `asz repack` keeps a session's idle clock. A file it cut kept the time of the first file its
  records came from, and the files were numbered stream by stream, so the file with the highest
  sequence, where retention reads how long a session has been idle, need not be the newest.
  Retention could then remove a session's files in the new root, or retire it, while the root it was
  repacked from kept everything. Each new file now has the header of the newest file its records
  came from, and the files are numbered in the order their records landed.
- `asz repack` cuts a file by what it takes once written, its header and closing line included, as
  the collectors do. It counted only the record lines, so a file of several records could be larger
  than `max_delta_bytes`: at a budget of 1,024 bytes, three records made a file of 1,338.
- `asz repack` carries every cursor. A stream can have more than one source, such as two recorder
  directories, and each source after the first keeps a cursor of its own, `<kind>-<hash>.cursor`.
  Only `<kind>.cursor` was carried, so collection into the new root read the other sources again
  from their start and landed their records again. Each cursor's `last_seq` now names the new file
  holding the last record its own source landed. A recorder inside the storage root writes its
  cursors' `origin` relative to it; a repack writes that origin, and the name of a cursor made from
  it, as a collector into the new root computes them, or that recorder was read from its start.
- `asz repack` cuts the records of two files into one only when the files are next to each other in
  their directory and their headers are the same apart from `seq` and `at`. A file took the header
  of the first file its records came from, so records of another source or another adapter version
  landed under the wrong header.

## Retention

- A storage root no longer only grows. `storage.retention.metadata_only_after`, `-1` and off by
  default, keeps only the metadata of a conversation that has had nothing new land for that long,
  such as `72h` or `30d`, to save disk: it removes the landed files, and a skeleton of each keeps
  who did what, when, in what order and at what cost. It applies whether the conversation was sent
  or not, and whether an endpoint is set or not; the content of one never sent is then gone
  everywhere. On one root, 7,235 landed files of 847.5 MB had skeletons of 29.6 MB, 3.5% of them,
  measured on 2026-10-13. `asz collect` and `asz server` remove at the end of a pass, after the
  send, the whole conversation's files at once, and only once its head round covers them and, with
  metrics derived, every file of it is derived. See [Retention](../formats/storage-root.md#retention).
- Metrics turned on after a conversation kept only its metadata count its tokens from the
  skeletons, which keep no execution record's protocol or server, so no MCP call is derived from
  them. The first pass of a process that saves its state says, for each session, how many distinct
  execution observations its skeletons keep that no pass counted, as `metrics: <session>: <n>
  execution observations lack retained details`. The count does not say how many were MCP calls, or
  why one was not counted. Only execution files are read for it.
- A provider body landed after a removal is cut exactly as with nothing removed. The cutter holds
  each removed body by its skeleton, and the session's store, under `provider/`, keeps the bytes of
  every chain tip at a removal and of each removed piece a later body cites, so the same pieces are
  cited and the same tip is copied. A removal keeps the tips before it writes its intent, so a tip
  it cannot keep leaves the session's collectors landing. A body's skeleton keeps every segment of
  its manifest but the literal strings, so a body the session did not accept is not accepted from
  its skeleton either, and the index reads the same keys from it. `asz verify` rebuilds every body
  that is not removed from them, and reads the bodies again when a removal changed the session
  while it read them. With every scenario run twice, once removing everything after each
  send, bodies under a budget too small for a piece included, every body landed as the same record.
- Each removed file is replaced by a skeleton, `<name>.sk`: Session Data, compressed with gzip,
  that keeps every record and part in its place with every identifier the index reads, and none of
  the content, each part saying `removed`, or `too_large` where it was. Its header records the file's digest, size and record
  count, when and by which removal it was removed, and what the receiver confirmed of it. The index
  rebuilt from skeletons is the index rebuilt from the landed files, byte for byte, so a
  conversation that becomes active again continues the same chain. With every scenario run twice,
  once removing everything after each send, the receiver got the same files, rounds and metrics,
  byte for byte. A skeleton is never sent. See [Skeletons](../formats/session-data.md#skeletons).
- A removal writes `removing.json` first, then the skeletons, each synced with the error of its
  first sync checked, before it deletes a landed file, and only once its skeleton reads whole and
  records the file's digest. The directories the deletions changed are synced before the intent
  goes, and the session directory after it. A crash or a power loss at any point is finished by
  the next pass before anything lands,
  and no collector lands into a session while its removal is unfinished. A skeleton takes its
  file's modification time. A removed file's metrics request goes once the receiver has it, on the
  first pass after it is sent while retention is on; its receipt stays, so a deriver whose state
  a power loss took back derives the file as before.
- Every reader takes a skeleton. `asz verify` checks lines and bytes through it, binds rounds to
  the digest it recorded, and reports removed files apart, never as verified. `asz show` says a
  record was removed. The page keeps what it read for the chain head and the removal generation,
  in the new `removal.state`, so one served by another process sees a removal too. It lists a
  removed file with `state: removed` and the size and digest it had, and draws its steps with no
  text and the state `removed`. A change record keeps its step, its identity, its time and each
  file's path, operation and lines, with no diff, and an execution record its step, identity, time
  and outcome, so the page lists both in their places, `removed`. Metrics
  turned on after a
  removal count the tokens a skeleton keeps, and no MCP call, whose record it does not keep.
  LangSmith's next position goes on from a skeleton. `asz repack` refuses a session with skeletons.
- The page says in its header how many of a conversation's files retention removed, and that only
  a receiver that accepted them still has their text. Its Prompt tab draws a provider body landed
  after a removal. It says that a removed body, or one built from it, cannot be rebuilt when nothing
  of it was kept, or that a piece of a body was too large to be stored, rather than that the body
  failed. The files route serves a removed file as its skeleton, with what the session's store kept
  of it under `kept`, and a file a removal deletes while the route reads as its skeleton too. A
  file that does not fit the route's budget on its own is served alone, and the page asks again for
  what an answer left out. The renderer is Horizon's, now pinned at
  apache/skywalking-horizon-ui#192, whose prompt store holds skeletons, takes a piece by the SHA-256
  a removed or too large part keeps, from the record that landed first, and reads kept bytes.
- A conversation can be retired. `storage.retention.retire_after`, `-1` and off by default, is how
  long a conversation may be idle before everything of it is removed, its metadata too, and it is
  retired. A retired conversation is never collected again, even when its runtime resumes it. Set
  `retire_after` longer than the runtime keeps its own history. Idle is read on the same clock
  as `metadata_only_after`, which must be set too, and `retire_after` is at least as long; equal,
  a conversation is retired with no metadata-only step first. Either rule broken is a
  configuration error. Only both settings off keeps everything. `asz collect` and `asz server`
  retire at the end of a pass, after the send, a conversation whose head round covers its files
  and, with metrics derived, every file of which is derived. See
  [Retirement](../formats/storage-root.md#retirement).
- A retirement writes its marker, `_retired/<session-id>`, an empty file, and syncs it before it
  deletes anything. Next, it writes again, from its receipt, each metrics request the receiver does
  not have, and syncs it. It then moves the session's directory and its conversation's under
  `_removed/` and deletes them there. It syncs every directory it changed before it takes the
  session out of `push.state` and `metrics.state`. The provider's table keeps its lines, marked
  `retired`. Requests not sent stay until they are sent, to the session's owner only, whose line
  stays meanwhile. A crash or a failed sync at any point is
  finished by the next pass before anything lands, whatever the settings say then, and `asz parse`
  and `asz push` finish it first too. The pass line counts `retired=`, and the startup says what
  each level does.
- Nothing of a retired session is written again. Every collector passes it over before it makes a
  directory, the provider's bodies of it included, and a LangSmith run of it never waits. The
  metrics deriver and the parser do nothing for it, `asz push` sends none of its files or rounds
  but still sends its waiting metrics requests, and `asz repack`, an sd scenario build and the
  scenario runner refuse it. The pass log says once a process that a retired session's source was
  found and not collected, `asz sources` names it, and `asz verify` counts the retired sessions.
- With `retire_after` set, asz reports what it could inspect in Claude Code's settings.
  It says that effective retention was not determined. It names invalid or unreadable settings
  and assumes no value for them. It warns when retirement is shorter than the inspected value or the reported
  default of 30 days. Kiro always warns when retirement is set. These lines appear at startup,
  in `asz sources`, in the list page's status, and on standard error in the first pass only.
  See [Retirement](../formats/storage-root.md#retirement) for the inspection's limits.

## Configuration

- The `view` section is read strictly. A key it does not have is refused, where every other section
  ignores one, because a misspelled key there would show everything to everyone. `hide` is read only
  inside the `view` section, and written anywhere else it is ignored and withholds nothing.
  `view.hide_override`, off by default, lets a request's `hide` parameter replace `view.hide`. Only the
  first YAML document of a file is read, so a file with a later one that holds anything is refused
  rather than read in part. The rule is on the [configuration page](../setup/configuration.md#view).

## Metrics

- Metrics are derived only when they will be sent: `export.otlp.endpoint` is set and
  `export.otlp.metrics` is on. A root with no endpoint derived every metric on every pass and kept
  each request in its spool until it was sent, which was never, so the spool only grew. Such a root
  now derives nothing, and the pipeline's first lines say so. `asz push` derives what the root has
  not derived yet before it sends, under the export lock, so a root collected with no endpoint and
  pushed later still carries its metrics; a file it cannot derive fails the command. One derivation
  runs at a time over a root: two at once, of different files of one series, could each decide a
  window from the state as it was and send windows that overlap. A root that never derived starts with a first derivation, bounded by
  `metrics.lookback`; one that derived before goes on from what it derived.
- Metrics are one section of the configuration, for the whole root. `metrics.enabled`, on by
  default, derives every metric from every landed file, whichever adapter landed it.
  `metrics.lookback`, 72 hours by default, bounds the first derivation over a root that already
  has history. The adapter keys `metrics` and `metrics_lookback` are gone, and a file that still
  writes them has them ignored. Metrics used to be off by default, with a look-back of 24 hours.
- `claude_code.token.usage` is now `agent.token.usage`, with the same description, unit, kind and
  labels. The family is asz's and is meant for any agent, and a runtime's vocabulary stops at its
  adapter. A receiver's rules must read the new name.
- Two new metrics count the calls to MCP servers, from the plugin's execution records:
  `agent.mcp.calls` and `agent.mcp.duration`, by server, tool, the source of the server's
  configuration, outcome and query source. A server and a tool together name the target a call
  reached, which a receiver can treat as an endpoint of the agent.
- The `claude-code-otlp` receiver is removed. Every metric asz sends is derived from the landed
  files, so one root has one source for each count. The exporter's labels a transcript does not
  carry, such as the user and the organisation, and its other metrics, such as cost, are no longer
  sent. Point Claude Code's exporter at the receiver that reads it: the SkyWalking OAP reads its
  cost, active time, sessions, lines of code, commits, pull requests and edit decisions, and takes
  the tokens from asz. A configuration that still names the adapter loads, and an enabled entry is
  skipped with a line that says so. The spool holds only derived requests, and `spool.state` is no
  longer written.

## Development

- The `all-kinds` scenario holds a call to an MCP server, so the push to a real OpenTelemetry
  Collector carries an `execution` file and the MCP metrics, and the check compares them with the
  root.
- The first benchmark: `BenchmarkLangChainRequests` measures cutting every request body of one
  LangChain conversation, for context windows from 8K to 1M tokens, trimmed, summarised or neither.
  `make test` and CI only compile it. Its comment gives the command that runs it, the reference
  numbers, and the machine they come from.
- A scenario's `error` step can name no call, `error: {text, unidentified: true}`, and an
  expectation can check the session's title, `session: {title}`. `all-kinds` checks that a child's
  meta file does not rename the conversation.
- golangci-lint is v2.14.0. Go 1.27.2 writes export data version 5, which v2.13.2 cannot read, so
  under it lint stopped at the first package with a type-check error. CI takes the newest Go 1.27
  release, so CI failed as soon as 1.27.2 came out.
- Two tests in `tests/chain` cover an upgrade cut into several rounds: one loses
  `conversation.state` after the first round and the next parse finishes from the rounds on disk;
  in the other, a round written by hand removes a node and a relation, adds others and an open
  unresolved entry, and changes a node, and the next parse, one frame a round, adds, removes,
  revises and resolves until the conversation is what the evidence builds.
- The verify package's record digest field, never filled, is removed. A record's `sha` is the
  digest of its source bytes, which the storage root does not hold, so only each file's closing
  digest and each provider body's digest can be checked, and both are.
- A scenario's tool result and queued message can be padded, `result: {text, pad}` and
  `queued: {text, pad}`, to carry more than a landed file's budget without spelling it out. An
  expectation counts the document's steps by content state, `view: {states}`, and its change records
  by theirs, `view: {change_states}`. A new property,
  `files_fit_the_budget`, holds every landed file of every scenario to the budget.
  `parts_keep_source_bytes` checks that a part taken out names the digest of a value of its source
  record, and rebuilds a provider body that took a piece out from the file the runtime wrote, and
  `records_match` compares the size and digest of a part taken out or shrunk across the two formats.
  The `too-large` scenario returns a 40,000-byte result, queues a 30,000-byte message, and writes two
  30,000-byte files, one through a shell command the plugin observes and one through the runtime's
  own Write tool, under a 16 KiB budget. The scenario's Session Data writer now gives an editing
  tool's result the runtime's own record of it as data, as the adapter lands it; it gave such a
  result the shell's shape before.

## Conversation model

- A call says what its history was compared with, and a card stands where it changed. Where a
  runtime records its requests, Claude Code with provider bodies and LangChain, each call's history
  is the message list its request recorded, built from the provider thread where a request
  continues one, and is compared with the call before it in its stream and epoch. A call carries
  `history`: `kept`, `changed`, `first`, `not_compared`, or `unavailable` with the reason, and the
  earlier reply's status. A changed history draws a `history.change` step just before the call, with
  what was kept, changed, dropped and added, and its cause `unavailable`: it is no context reset, and
  the calls stay in their epoch. The rules are on the
  [Session Flow](../formats/session-flow.md#where-a-calls-history-changed) page. On every LangChain
  capture and on the Claude Code 2.1.302 capture, no history changes without a mark, so no card is
  drawn; the scenarios `history-forget` and `history-threads` hold a trimmed history and a thread.
  `provider_bodies_leave_the_fold` now lets provider bodies add the card, and nothing else.

- Model output whose record names no provider call is a step of its run. It was in no node at all,
  because only a call held text and reasoning. No Claude Code or LangChain record lacks the call;
  every Kiro record does, since Kiro ties no record to a request. Grouping such output into calls
  by turn or by record would draw boundaries the runtime never recorded, so no `llm.call` is
  written for it. The scenario `output-without-call` holds it.
- A sidecar of the `main` stream names the conversation: its label is the session's title, as a
  title record's is. A sidecar used to name only the stream it described. Kiro keeps a session's
  title in its header, which lands as the main stream's `agent_meta`.
- `control.notice` is in `model.Vocabulary`, the list every glossary is checked against. It was the
  one kind the model emits that the list left out, so a glossary could miss it with no test failing,
  and the Kiro and LangSmith glossaries did.

- A stream whose first record is a reset now has one epoch, opened by that reset. It used to get an
  empty epoch before the reset, with the reset's own id, so one epoch replaced the other and
  followed itself. No Claude Code stream starts with a reset. A LangChain agent whose first model
  call was sent a summary does.
- A stream a call started now leads back to it. A sub-agent does a task and hands the result back,
  and the model recorded the way in, `starts`, with nothing for the way out, so every child read as
  a dead end. A child's `agent.output`, and an auxiliary stream's last model call, now have a
  `result_of` relation to the call that started the stream, with the result that call received as
  its evidence, and the child's `returned_value` is observed where it said unavailable. It is
  written only when it is exact: one call started the stream directly, and its result is its own
  and not the acknowledgement of a background launch. A background child keeps its way back through
  the notification, `reports`, and a child that never came back has no `result_of`. LangChain's
  nested agents and Claude Code's synchronous returns gain it; nothing is re-landed, and the next
  round of an existing conversation adds it.

- The document lists what happened in the order it happened, never by id: a stream's `opened_by`,
  a step's `edges` and the `relations` list by the position of their records inside one stream or
  run, and by time across streams; workspace changes and tool executions of one time by where each
  was read. A workflow launch listed the streams it started by the hash in their ids; on real data,
  130 of 150 steps that started several streams led with a different stream than their run's first.
  A stream's origins had been listed in the order a Go map gave them, so the document changed from
  one read to the next: on a real conversation whose stream had three, five reads gave three
  orders.

- On one record, a node on the whole record comes before the nodes on its parts, and those follow
  their block. The two used to be decided by id, which made a cycle, so a sort could return the
  siblings in any order from one read to the next. The OAP already ordered them this way.
- The page reads a record's time only up to the first line of a file that does not decode, as
  Session Data says a reader stops there; it read the times of the records after it. Reading them
  now decodes each record, which on a 142 MB session takes about 0.25 s more. A file's first and
  last time, and a node's, keep a time before 1970, which the last time used to lose, and no
  longer depend on the order Go gives a map when a record is at exactly 1970-01-01T00:00:00Z.

- Segments are activity windows only. A segment node carries no attributes: its talks are its
  `in_segment` relations, and its time range is theirs. `state`, `gates_unmet` and `committable` are
  gone, with the four gates that set them. A round carries no clock, so the last window of every
  session stayed `open` for ever, even in a session quiet for weeks, and nothing committed a
  segment or read its state. In `asz.view` a segment keeps `id`, `talks`, `from` and `to`; whether
  the last one is still going is for a reader, who knows the time. The document keeps the version
  `1.0` although it no longer has two keys that 0.5.0 wrote. This is the one exception to the rule
  that a 1.x never removes a key, because no released SkyWalking OAP or Horizon read the document
  yet, and the [asz.view](../formats/asz-view.md) page says so.
- A round no longer repeats what it says elsewhere. Gone from the nodes: the session's
  `conversation`, which the header names, and `title_from`; an epoch's `records`; a talk's `loops`
  and `runs`, which its runs are; a talk's and a run's `trigger` (on the main stream a talk's first
  run is external and every later run a notification, and a child's talk is unknown); a call's
  `fragments`, which its `refs` are; and a tool's `name`, which its call part carries. A provider
  body's manifest no longer repeats a request's `run`, which is the record's own, or `why`, which is
  the text of the part that holds the body whole. The fixture conversation's round is 6% smaller.
  The header's `bash_runs` is counted as before, from the tool calls assembly made.
- The next round of an existing conversation revises each session, segment, epoch, talk, run, call and
  tool node once, without the dropped keys. Nothing is landed again, and the parser and policy stay
  the same.
- Revisions that need no new evidence go first, cut to the round budget by their frames. A round
  cannot be narrowed below one landed file, so on a long conversation an upgrade's revisions made one
  round past `max_round_bytes`: 12.5 MB for 35,434 nodes on a 17 MB chain, more than the 10 MiB one
  write may be in a BanyanDB cluster. They are now rounds of their own over no new file, six of at most 2 MiB
  there, and the round of the new evidence follows.
- The Claude Code provider adapter extends only an index that agrees with its saved state, as the
  other adapters do. A pass that wrote the index and stopped before saving its state made the next
  pass index the same files a second time, out of landed order.
- A round cut by `max_round_bytes` reads nothing past its watermark, not even through a lookup.
  Finding the record a reset continues from, or the run a record's parents lead to, could reach
  records past the round's `through_seq`, so the round could hold structure its own header and input
  digest did not cover. Assembly now works on the index as it stood at the watermark.

- Two different entities could get one id. A `/` inside a part was written as `_`, so the tool
  ids `a/b` and `a_b` gave one node, and one call took the other's place. An id nested in another
  wrote its `/` as `_` too, so the talk of the main stream's cycle `a`, `talk/main/a`, and the talk
  of a child stream named `main_a`, `talk/main_a`, gave their runs of the cycle `a` one id, and
  their `in_segment` relations too. And the epoch a reset opens was named by the reset's own id, so
  a reset whose id is `0` took the stream's first epoch's id and followed itself, and one whose id
  is `at_1_2` took the id of a reset with no id at sequence 1 row 2. Now:
  - inside a part, `%` is written as `%25` and `/` as `%2F`;
  - an id nested in another, a run's talk or a relation's two ends, has each `%` written as `%25`,
    each `_` as `%5F`, and then each `/` as `_`;
  - the epoch a reset with an id opens is `epoch/<stream>/id/<id>`, one opened by a reset with no
    id is `epoch/<stream>/at/<seq>/<row>`, and a segment is `segment/at/<seq>/<row>`, so the
    location stays parts of its own and nests without a `_`.

  The rules are on the [Session Flow](../formats/session-flow.md#entities) page. Parsed from scratch
  with and without the change, the 65 conversations of a local root, Claude Code and LangChain,
  hold 357,841 ids, and 3,637 of them moved: 2,791 `starts` and 28 `result_of` relations, because
  the tool use id at one end holds a `_`, 748 segments, and 35 epochs that a reset with an id opens,
  with their 35 `follows` relations. Nothing else moved. The next round of an existing conversation removes the
  moved ids, writes them again under the new ones, and revises the `in_segment` relations that end at
  a segment; nothing is landed again. The document's `sessions` are now the ones the round headers
  name, never one read back out of a session node's id, which writes a `%` as `%25`.
- A record that names the stream which started a nested child, as a Claude Code sidecar's
  `parentAgentId` does, names no call, but it was counted as a second call that started the child.
  A child that such a record named, and whose call received its result, got no `result_of`, and its
  `returned_value` said unavailable. The record now agrees with the call when that stream made the
  call, and a record that names another stream leaves the relation out, whether that stream landed
  or not. No conversation measured changes: in a local root, no record of a Claude Code sub-agent's
  transcript names a child, and each of the 31 nested children is named by its sidecar alone or with
  a notification.
- A round with an empty line in it verified. The reader skipped empty lines before it computed the
  digest and before it checked that nothing follows the commit, so a blank line added anywhere kept
  the round valid. [Session Flow](../formats/session-flow.md#reading-it) says a reader rejects a
  line that does not decode and a line after the commit, and the reader now does. asz never writes
  an empty line: none of the 948 rounds of a local root holds one.

## Release

- The `pypi` skill says what a release manager sets up before an upload. PyPI requires two-factor
  authentication. The upload that creates a project needs a token scoped to the whole account.
  Every later upload uses a token scoped to the project. The token is read from `SW_PYPI_TOKEN`,
  exported before Claude Code starts.
- The `pypi` skill downloads from downloads.apache.org first, because archive.apache.org still had
  no 0.5.0 fifteen minutes after the move. It verifies with `gpgv`, which leaves the user's keyring
  alone. After the upload, it checks that PyPI holds the files it built.
- The text `publish` writes for the GitHub release says the downloads page links the source package
  and the binary archives. It said each package, and the page does not list the Debian packages.
- The release guide lets `--remove-old` go ahead without a Scoop bucket when the project has none,
  and asks that archive.apache.org holds each version before it is removed.
- The `homebrew` skill no longer runs `git rm` inside the temporary tap, where git stops the check
  whenever `asz.rb` moves. Its template fits 0.5.0 and later only.
- The `apt` skill installs every older version in the index through the redirects, not only the
  versions added. Adding a version moves the one that was newest to the archive, so its redirect
  changes too.
- The release guide's commands for preparing a candidate by hand left the LangChain plugin's
  version in `plugins/langchain/pyproject.toml` as it was. `prepare` sets it to the version being
  released, so a candidate prepared by hand would have built PyPI packages of the previous version.
  The commands now set it, write the next version's changelog as `prepare` does, and push and open
  the pull request. In a clone at 0.6.0, they made the same two commits, tree for tree, as
  `prepare --no-push`.
- The release guide named the Debian package of the change recorder `asz-claude-code`, its name
  before 0.5.0, in the vote mail and where it says what each package installs. The Makefile builds
  `asz-changes`. The vote mail in the guide also lacked the note on the Debian packages that
  `candidate` writes, and now gives the lines on the verified CI run as `candidate` writes them. A
  test holds the guide's list of packages in the vote mail to the Makefile. The guide also says
  that `candidate` needs Go to read the Debian packages, and lists perl and cmp among the tools.
- `candidate` checks for `cmp` and `publish` for `perl` before they change anything, as they do
  for every other tool they run. `publish` lists the upload to PyPI with the `pypi` skill among the
  steps it prints after the move, between apt and Scoop, as the release guide orders them.
- The container image owns its storage root, `/asz/data`, as the user it runs as, 65532. The image
  had no such directory, so Docker made a new named or anonymous volume root's, and the first
  collection failed with `mkdir /asz/data/_root: permission denied`. A new volume now belongs to the
  image's user. A bind mount works as before.

## Documentation

- [Session Data](../formats/session-data.md#reading-a-record) and [Session Flow](../formats/session-flow.md)
  say a reader may refuse a line that holds more than 256 objects and lists open at once, as the
  OAP does. The deepest real line measured holds 16.
- [asz.view](../formats/asz-view.md) listed `main` and `child` as the roles of a stream. The
  document also carries `auxiliary`, a plain model call the agent made inside its own work, and the
  page now says when a stream has each of the three.
- [asz.view](../formats/asz-view.md) said the document carries no RFC 3339 strings. A workspace
  change and a tool execution keep their record's `time` as that string, and the page now says so.
  It also says how siblings on one record are ordered. [Session Data](../formats/session-data.md)
  and [OpenTelemetry export](../formats/otlp.md) list `execution` among the kinds a file can have.
- The format pages state every rule a reader needs to build the same document, so a reader in any
  language follows the pages and never asz's code. [Session Data](../formats/session-data.md#reading-a-record)
  gives the type of every record and part field, what a reader does with a line of another type, and
  how times are written and compared. [Session Flow](../formats/session-flow.md) says that ids and
  `attrs` keys sort in code point order, and when a call's `provider_bodies` makes a round
  unreadable. [asz.view](../formats/asz-view.md) says that times round down to the millisecond,
  that `attrs` and data are as written, what white space is, which record gives a call its result,
  and how deep a tree is written.
- The Claude Code adapter page said a `claude_code.tool.execution` span from Claude Code's
  OpenTelemetry export supplies a tool's execution time. asz keeps nothing of that export, and no
  such span was measured, so the page now says the export may carry it and that this is unmeasured.
  For a call to an MCP server, the plugin's execution record carries the measured time.
- `source_root` of `claude-code-local` was described as the directory Claude Code keeps its files
  in, `~/.claude`. It is the `projects` directory inside it, which is what the collector reads and
  what the container image page sets. Set to `~/.claude`, the collector found no sessions.
- The setup pages no longer carry upgrade guides from one version to another. The steps for
  upgrading from 0.4.0, and the notes about the names 0.5.0 replaced, are gone. The changelog of
  0.5.0 still lists every rename. Each way to install still says how to move to a newer version,
  and the Claude Code plugin keeps its upgrade steps, whose order keeps the plugin's data.
- [LangChain and LangGraph](../setup/langchain.md) said a LangChain conversation has no context
  reset. A summary that `SummarizationMiddleware` marked now resets it.
- The LangChain plugin's README, which is its page on PyPI, says the plugin needs `asz-changes` and
  a `tools.scope` naming the application's tools. It links the documentation of the released
  version.
- [Export over OpenTelemetry](../setup/export-otlp.md) showed an OpenTelemetry Collector
  configuration with a logs pipeline only. asz sends metrics by default, and that Collector refused
  them, with `Unimplemented` over gRPC and `404 Not Found` over HTTP, so `asz push` failed. The
  configuration now has a metrics pipeline too. CI's Collector check kept its own copy, which had
  the metrics pipeline, so it passed. It now reads the configuration from the page.
- The contributing guide and the docs index pointed to this repository's issues, which are turned
  off. They now point to the SkyWalking issue tracker, which the SkyWalking projects share.
- The Claude Code adapter page said the system prompt and the tool schemas are absent from
  transcripts. That holds for the versions it was measured on, 2.1.220 to 2.1.251, and the page now
  says so. Later versions write them in two attachments, which the adapter names.
