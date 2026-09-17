# Claude Code Provider Bodies

Claude Code can write every request it sends to its model provider, and every response it gets
back, to a directory. A request holds what no transcript records: the system prompt, the tool
schemas, the reminder blocks injected into the first message, the `role:"system"` messages placed
between the others, the cache markers, the betas and the thinking settings. The
`claude-code-provider` adapter collects those files, finds the session of each, and lands them
beside the session's transcripts. The view joins each body to the call it belongs to.

Everything on this page about what Claude Code writes was read from the 2.1.260 code and measured
on two captures of that version, unless it says otherwise.

## Turn it on

Add one line to the `env` block of `~/.claude/settings.json`, with the directory the adapter reads:

```json
{ "env": { "OTEL_LOG_RAW_API_BODIES": "file:/Users/me/.claude/asz/provider-bodies" } }
```

- **The path must be absolute.** Claude Code does not expand `~`, and it resolves a relative path
  against each session's own working directory, so a relative path writes a directory into every
  project.
- **The directory** the adapter reads by default is `asz/provider-bodies` under Claude Code's
  directory: `CLAUDE_CONFIG_DIR`, else `XDG_CONFIG_HOME/claude`, else `~/.claude`. Set
  `source_root` on the adapter to read another, and name the same directory in the variable.
- **No telemetry is needed.** The capture wrote the files with no other `OTEL_*` or telemetry
  variable set.
- **The adapter is on by default** in `asz.yaml`, and does nothing while the directory does not
  exist. Setting the variable is the choice to collect.

`asz sources` says what the adapter sees: the directory, how many requests and responses it holds,
and which sessions the requests name.

## What the files hold

Claude Code writes one file per body, every session into the same directory.

| | Request | Response |
| --- | --- | --- |
| Name | `<random UUID>.request.json` | `<provider request id>.response.json`, or a random UUID when there was none |
| Content | the request as it was built: `model`, `messages`, `system`, `tools`, `betas`, `metadata`, `max_tokens`, `thinking`, … | the last assistant message, with every content block of the streamed message |
| Session | `metadata.user_id` is a JSON string holding `session_id` | none |
| Joins | the first system block names `cc_prompt_id`, the prompt, and `cc_prev_req`, the request id of the previous call of its chain | the name is the request id; `id` is the message id a transcript records |

Thinking text is replaced by `<REDACTED>` in both. Claude Code writes a file with one asynchronous
write, with no temporary file and no rename, and it never deletes one.

**What is in them besides the conversation.** A request holds the device id and the account id in
`metadata.user_id`, the person's email and the date in the reminder block, the whole instruction
files and memory index, the schemas of every MCP tool, and the environment block with paths and
the operating system. asz lands and sends the bodies as they are, and redacts nothing.

**How large.** In a capture with the full tool list, a main request was 121 to 196 KB, of which the
tool schemas were 74 KB and the system prompt 10 KB, and a subagent's request 33 to 58 KB. Every
call sends its chain's whole message list again. So most of a body is bytes the session already has.

Earlier messages come back with the same text, but not always spelled the same way. Two things move
as the list grows. The cache marker, `cache_control`, moves to the newest message. And a message of
one text block is sent as a list of blocks while it is the newest, and as a plain string once the
marker has left it, so the same message has two spellings depending on where it sits. A reader
comparing one request with the next has to treat both spellings as one message: comparing them as
written reports a history that merely grew as one that was rewritten.

A compaction is the case that really does rewrite the list. It replaces the context with a summary
and starts the list again, so the list is shorter as well as different, and the conversation records
it as an `epoch.boundary` step with the summary beside it.

## What the adapter lands

**A session for each body.** A request names its session; a session id that is not in Claude Code's
own form is not taken, since it names a directory. A response is claimed by a later request that
names its request id, or by the one session whose landed transcript holds its message id. The
transcripts searched are those of sessions with a request written within 24 hours of the response,
so a response whose session has no collected request waits. Anything else waits, and every pass
tries again. In the capture of 15 calls, 13 responses were claimed. The other two, the call that names the session and the compaction call, have no call in
any transcript and are never landed. A body that waits is counted in the pass and does not make the
pass fail. The pass line counts both, as `bodies_waiting` and `bodies_unreadable`.

The adapter runs after `claude-code-local` in a pass, so a response only a transcript names finds
its session in the same pass. The session filter is the local adapter's: `include` and `exclude`
are judged by the directory the session's main transcript sits under. A session whose transcripts were
pruned since they landed is judged by the directories its landed transcripts name. A session nothing
names a directory for waits.

**Repeats taken out.** A body is cut into pieces: each tool definition, and every JSON string of
1 KiB or more. A piece the session already holds becomes a reference to it, and the front a body
shares with an earlier body of its chain becomes one copy of that front. What is left lands as
literal bytes. On the two captures, the landed files were 25.5% and 21.6% of the bytes Claude Code
wrote, and all 42 bodies rebuilt to the files' exact bytes.
[Session Data](../formats/session-data.md#provider-bodies) describes the record.

**One file per pass.** Every new body of a session in one pass goes into landed files under
`<session>/provider_body/`, a file ending before a body that would take it past `max_delta_bytes`; a
single body larger than that lands alone. A body names no stream, so the file is filed under
the session and the view decides which stream and call a body belongs to.

**Checked before it lands.** Each record is written, read back and rebuilt before it lands. A body
that is not one JSON object in valid UTF-8, or does not come back whole, lands whole in one unknown
part, and its manifest says why. `asz verify` rebuilds every landed body and compares its digest.

A file changed after it landed is a conflict: nothing more lands from it, and every pass fails while
it stays, so a single pass of `asz collect` exits non-zero. Putting the landed bytes back ends it. A
file that is not one JSON value two minutes after it was last written is counted as unreadable.

## In the view

In the [asz.view](../formats/asz-view.md#provider-bodies) document, a call lists its request and its
response under `provider_bodies`, each by the landed record it rebuilds from. The document does not
carry the bodies. A reader loads them when asked: the session's `provider_body` files up to the one
named, read once and kept, since a body can refer to earlier files. A response joins by its message
id. A request joins to the
call whose previous call's response carries the request id the request names, and whose prompt is
the one it names, when exactly one request and one call carry those two ids. On the capture, all 13
calls had their request and their response joined, the subagent's included. The request that names the session, and the compaction request, which names no
prompt, were left unjoined.

## Keeping the directory small

Claude Code never deletes these files, and asz never deletes a source file of a real session. The
capture wrote 1.7 MB for 15 calls. Delete old files yourself once they are collected, for example
every file older than 30 days:

```sh
find /Users/me/.claude/asz/provider-bodies -name '*.json' -mtime +30 -delete
```

A body deleted before a pass collected it is lost. The pass line counts the bodies still waiting.

## Not measured

Subagents or workflow agents running at the same time, server-side clearing of old tool results,
the `thread` field that sends only a tail of the messages, retries, a long session, `--resume` and
`--fork-session`, Bedrock, Vertex and gateways, and Windows.
