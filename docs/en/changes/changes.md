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
  here and for the first call of any chain, where nothing is missing. The renderer is Horizon's, now
  pinned at `88e0110`, which also draws a light theme's scrollbars and other native controls light.

## Claude Code

- A headless run's question is on the page. `claude -p` and an Agent SDK application write their
  prompt with `promptSource: "sdk"` and no `origin`, and only `origin.kind: "human"` was read as a
  person's input, so the page showed each answer without the question it answered. Such a prompt
  is now external input that opens its talk. A session collected before this keeps the gap, because
  landed records are never rewritten.

## MCP calls

- A call to an MCP server names its server and its tool. Claude Code calls an MCP tool
  `mcp__<server>__<tool>`. A landed call part now carries `server` and `server_tool` when the name
  splits exactly, with one `__` after `mcp__`, and the step's `attrs` carry `mcp_server` and
  `mcp_tool`. A name with a second `__` is left whole, since the separator cannot be told apart
  from a name that holds it. A session collected before this has neither, because landed records
  are never rewritten.
- The Claude Code plugin records each call to an MCP server. Its hooks after every `mcp__` call
  write an `execution/1` record: the server and where its configuration came from, how the call
  ended, the time Claude Code measured around it, and the size and SHA-256 of the arguments and of
  the answer, never their text. The `changes` adapter lands them as a new kind, `execution`.
  Assembly does not read them. `asz.view` lists them as `tool_executions`, and a step names its
  records in `executions`, joined by the tool-use id. `mcp.enabled: false` in the plugin's
  settings turns them off. The shapes were measured on Claude Code 2.1.282, and the real hook
  payloads are the plugin's test data.

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
