# Changes in 0.6.0

> In development, not yet released. `tools/release/release.sh prepare 0.6.0` removes this note.

## Development

- The first benchmark: `BenchmarkLangChainRequests` measures cutting every request body of one
  LangChain conversation, for context windows from 8K to 1M tokens, trimmed, summarised or neither.
  `make test` and CI only compile it. Its comment gives the command that runs it, the reference
  numbers, and the machine they come from.

## Conversation model

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
