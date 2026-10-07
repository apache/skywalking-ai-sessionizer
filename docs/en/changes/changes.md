# Changes in 0.6.0

> In development, not yet released. `tools/release/release.sh prepare 0.6.0` removes this note.

## LangChain and LangGraph

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
  captured corpus no record is named: the client sends both out of band, and all 46 trace roots are
  chain runs.

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
  used to show the step's text under every position. The renderer is Horizon's, now pinned at
  `c903e77`.

## The page

- A view can withhold the system prompt and the tool schemas. An operator who serves a conversation
  to the people an agent served may need to keep from them what the runtime sent the model, while
  the operator still sees it. asz knows nothing about who is reading. It withholds by the flags the
  adapter set, never by the text or the size of a part. `view.hide` in the configuration lists the
  flags that `asz view` and `asz server` withhold from every reader. A `hide` parameter on the
  document, record and files endpoints adds names for one request and never takes one away. A host
  that serves the API through its own route adds it to every request, and so decides per reader. The
  page passes a `hide` in its own address on to its document, record and files calls.
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

## Configuration

- The `view` section is read strictly. A key it does not have is refused, where every other section
  ignores one, because a misspelled key there would show everything to everyone. `hide` is read only
  inside the `view` section, and written anywhere else it is ignored and withholds nothing. Only the
  first YAML document of a file is read, so a file with a later one that holds anything is refused
  rather than read in part. The rule is on the [configuration page](../setup/configuration.md#view).

## Metrics

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
- The `claude-code-otlp` receiver no longer keeps what Claude Code's exporter sends. It accepts
  metrics, logs and traces, over gRPC or over HTTP in any encoding, and keeps none of them. Its
  `metrics` key is gone. Every metric asz sends is derived from the landed files, so one root has one
  source for each count. The exporter's labels a transcript does not carry, such as the user and
  the organisation, and its other metrics, such as cost, are no longer sent. The spool holds only
  derived requests, and `spool.state` is no longer written.

## Development

- The `all-kinds` scenario holds a call to an MCP server, so the push to a real OpenTelemetry
  Collector carries an `execution` file and the MCP metrics, and the check compares them with the
  root. The same run sends a real exporter's metrics to the `claude-code-otlp` receiver, and fails
  if any of them reaches the spool or the Collector.
- The first benchmark: `BenchmarkLangChainRequests` measures cutting every request body of one
  LangChain conversation, for context windows from 8K to 1M tokens, trimmed, summarised or neither.
  `make test` and CI only compile it. Its comment gives the command that runs it, the reference
  numbers, and the machine they come from.

## Conversation model

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

## Documentation

- [Session Data](../formats/session-data.md#reading-a-record) and [Session Flow](../formats/session-flow.md)
  say a reader may refuse a line that holds more than 256 objects and lists open at once, as the
  OAP does. The deepest real line measured holds 16.
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
- The contributing guide and the docs index pointed to this repository's issues, which are turned
  off. They now point to the SkyWalking issue tracker, which the SkyWalking projects share.
- The Claude Code adapter page said the system prompt and the tool schemas are absent from
  transcripts. That holds for the versions it was measured on, 2.1.220 to 2.1.251, and the page now
  says so. Later versions write them in two attachments, which the adapter names.
