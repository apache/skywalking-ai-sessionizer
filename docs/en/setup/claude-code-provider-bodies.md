# Claude Code Provider Bodies

Claude Code can save every request it sends to its model provider, and every response. A request
holds what the transcript does not: the system prompt, the tool schemas, and every message as sent.
asz collects these files, and the conversation page shows the prompt of each model call.
[Claude Code Provider Bodies Internals](../adapters/claude-code-provider-bodies.md) explains what
they hold and how asz stores them.

## Turn it on

Add this to the `env` block of `~/.claude/settings.json`, with your home directory in the path:

```json
{ "env": { "OTEL_LOG_RAW_API_BODIES": "file:/Users/me/.claude/asz/provider-bodies" } }
```

- Write the path in full. Claude Code does not expand `~`.
- The directory is `asz/provider-bodies` under Claude Code's directory, which is
  `CLAUDE_CONFIG_DIR` when that is set. To use another one, set `source_root` on the
  `claude-code-provider` adapter to the same path.
- Nothing else is needed. The adapter is on by default.

Start a new Claude Code session, then check that asz sees the files:

```sh
asz sources
```

## What they contain

The bodies are sent to asz and to any [export](export-otlp.md) receiver as they are, and nothing is
redacted. Besides the conversation, a request holds your device id and account id, your email
address, your instruction files and memory, the schemas of every MCP tool, and paths on your
machine.

## Keep the directory small

Claude Code never deletes these files, and a session writes about 100 KB per model call. Delete old
files once asz has collected them, for example every file older than 30 days:

```sh
find /Users/me/.claude/asz/provider-bodies -name '*.json' -mtime +30 -delete
```

## Turn it off

Remove the line from `settings.json`, and delete the directory.
