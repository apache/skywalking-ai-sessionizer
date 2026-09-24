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

## Development

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
