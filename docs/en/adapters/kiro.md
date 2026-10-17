# Kiro

How Kiro maps onto the [unified conversation model](../concepts-and-designs/unified-conversation-model.md),
what the `kiro-local` adapter reads, and what Kiro's files cannot supply.

For turning it on, see [Kiro](../setup/kiro.md). For the words, see the
[Kiro glossary](kiro-glossary.md).

## Where the data comes from

Kiro writes each session to disk as it runs. The adapter reads those files, as the Claude Code
adapter reads Claude Code's, and needs nothing from Kiro: no setting, no hook, no plugin.

Kiro has three agent engines, and only one is read.

| Engine | Used by | Where it writes | Read |
| --- | --- | --- | --- |
| V3, `@kiro/agent` | `kiro-cli --v3`, or `kiro-cli` with `chat.agentEngine` set to `v3`; Kiro Crew on its `kas` backend; the Kiro IDE | `~/.kiro/sessions/<workspace key>/sess_<id>/` | yes |
| V2 | `kiro-cli` by default in 2.28.0 | `~/.kiro/sessions/cli/<id>.jsonl` | no: counted and reported |
| V1, `--classic` | `kiro-cli --classic` | a SQLite database beside the login | no |

Measured with kiro-cli 2.28.0 and its engine `@kiro/agent` 0.66.26, and Kiro Crew 0.7.2 on it, on
one macOS machine: 86 sessions, 56 of them with records, and 3,101 records, from tasks run for the
purpose and from real use. Kiro Crew 0.8.0, which bundles kiro-cli 2.27.1 and engine 0.66.22, was
measured on the same machine after it updated itself: 13 sessions, 10 of them with records, and
2,069 records, from real use. All 10 were collected and verified.
The engine's code was read where the files did not say enough, and each such point names it. The
IDE writes the same store; no IDE turn was measured.

A session is read when its header, `session.json`, says `schemaVersion` `1.0.0`, the one schema
measured. A session that says another is counted and left, as the V2 sessions are, and both appear
in `asz sources` and once in `asz collect`:

```text
not read : kiro-local: 24 session(s) of kiro-cli's V2 engine under sessions/cli; only the V3 engine's sessions are collected: start kiro-cli with --v3, or set chat.agentEngine to v3
```

Kiro keeps a session until a person or Kiro Crew deletes it. A session retired by
`storage.retention.retire_after` is never collected again, its sub-executions included, even when
Kiro resumes it. See [Retirement](../formats/storage-root.md#retirement).
asz warns whenever `kiro-local` is enabled and `retire_after` is set because Kiro keeps a session until it is deleted.

## What is read

One session directory holds:

| File | Read as |
| --- | --- |
| `session.json` | the session's header, replaced whole by the engine, also between turns. It lands as a new snapshot when its bytes change, as an `agent_meta` file of the main stream. Its `title` names the conversation. |
| `messages.jsonl` | the main stream, `main`. Only appended to. |
| `sub-executions/<id>.jsonl` | one subagent's stream, named by the file. Only appended to. |
| `tool-outputs/`, `snapshots/`, `publish*.cursor`, `session.json.tmp.*` | not read: a large tool output the engine spilled, the files it keeps for checkpoints, its upload offsets, and a header copy left by an interrupted replace |

A session with a header and no record is not collected until it has one. Kiro Crew opens sessions
before it uses them: 30 of the 86 held nothing else.

The engine writes under the home directory and does not read `KIRO_HOME`, so the default source
root is `~/.kiro`. The workspace key in the path is a digest of the session's workspace paths. The
paths themselves are in the header, and the [session filters](../setup/configuration.md#the-kiro-adapter)
match them.

## The mapping

| Model | Kiro |
| --- | --- |
| Conversation | the session id, `sess_<uuid>`; supplied, one session is one conversation |
| ExecutionStream | `main` for `messages.jsonl`; a child stream per `sub-executions/<subSessionId>.jsonl` |
| Run | `executionId`, one turn |
| Talk | a prompt and the turn it opened |
| `llm.call` | none: no record names the model request it came from |
| `message.external` | a `user` record |
| `message.assistant` | `assistant` with `operationType` `Say` |
| `thinking` | `assistant` with `operationType` `Reasoning` |
| `tool` | `tool_call` and `tool_result`, joined by `toolCallId` |
| `agent.call` | `sub_agent_start` |
| `agent.output` | the child's last response, with `sub_agent_complete`, the response the parent received |
| `context.injection` | `agent_note`, `session_start`, `steering_inclusion`, and a `user` record the engine wrote |
| `epoch.boundary`, `epoch.summary` | `tombstone`, and `assistant` with `operationType` `Summary` |
| `error.api` | `session_metadata` with `key` `displayError` |
| `control.notice` | a `turn_end` whose `stopReason` is not `end_turn`, a `ContextualHookInvoked`, and a record type the adapter does not describe |
| `turn.duration` | `usage_summary` |

### A prompt belongs to the turn after it

A turn is `turn_start` … `turn_end`, and every record inside names it by `executionId`. A
person's prompt does not: none of the 210 `user` records carries one, nor does the note the
engine writes before each prompt. A message sent while a turn runs, which the engine marks
`source: steer`, carries none either.

Each belongs to the first turn named after it in the file: the prompt to its own turn, and a
message sent during a turn to that turn. The rule reads line order, which the engine keeps. Its
timestamps are not in line order: the note before a prompt is stamped later than the prompt.

A record whose turn is not in the file yet waits. A landed record is never written again, so a
prompt landed before its turn starts would sit outside that turn for good. The adapter reads past
what it is about to land, and lands the waiting records once the turn is named, which the engine
does milliseconds later. A prompt whose turn never starts, because the engine stopped first, lands
with no turn once the file has been still for five minutes.

### Model output without a model call

The engine records what the model said and not which request said it. A turn lists its request ids
in `usage_summary`, and no record names one. Grouping records into calls, by turn or by record,
would draw call boundaries Kiro never recorded, so no `llm.call` is written. The agent's text,
reasoning and tools are steps of the run.

### Record ids

A record's id in Session Data is the engine's `id`, then `#`, then the first 12 hex digits of the
record's SHA-256. The engine can give one id to two different records: when it asks twice to
approve one tool call, both requests carry one id and both answers another. In the 3,101 records, 46
ids did so, for 23 approvals, and no other id was reused. Assembly removes a repeated id as a copy, so the engine's id
alone would drop the second record. The digest keeps the id the same for the same bytes, so a record
landed again after a crash is still removed as the copy it is.

### Subagents

The engine writes `sub_agent_start` and `sub_agent_complete` in the parent, and the child's records
in `sub-executions/<subSessionId>.jsonl`. No tool call in the parent names the spawn, so the start
record is the call: its id is the child's `subSessionId`, and `sub_agent_complete` is its result.
The `starts` relation joins the call to the child's stream, and `result_of` joins the child's output
back to the call.

A child's stream is named by its file. The child's records carry the parent's turn as
`executionId`, and most carry the child's id as `subExecutionId`, but hook records in a child do
not.

### Compaction

A `tombstone` takes the records before it out of the context the model is sent: `kind`
`summarization` after a compaction, `checkpoint_revert` after a return to a checkpoint. It starts a
new epoch. `effectiveFromMessageId` names where the dropped records begin, not the last record
kept, so the epoch names no continuation. The summary that follows is `assistant` with
`operationType` `Summary`, which the engine sends to the model as a person's message, so it is input
here. Nothing joins it to its tombstone, so no `summarizes` relation is drawn.

### The system prompt

`session_start` holds the system prompt the engine composed, and is named `system_prompt`, so a
server can [withhold](../formats/asz-view.md#withholding) it. The engine writes it when the first
turn ends.

Kiro Crew sends its own system prompt as the first prompt of a session, in a `user` record: 24 of
24 Crew sessions measured begin so. It sends its rules again in the prompt that resumes a session.
Nothing in the record tells either from a prompt a person typed, so each is a `message.external`
and is not named `system_prompt`.

### Messages sent during a turn

Three `user` records carry a message sent while a turn runs. The person's message, `source:
steer`, is a `message.external` of that turn. The engine's text that carries it to the model,
marked `syntheticUserMessageReason`, is a `context.injection` of the same turn. The note that the
messages were consumed, `steeringClearedIds`, is kept and is no step. Kiro Crew also sends its own
notices this way, such as one that it blocked a tool call; those read as the person's messages do.

### Reasoning

A `Reasoning` record whose text is `...` holds no reasoning: the engine wrote that in place of
reasoning it did not keep, and reads it as none itself. Its `thinking` step is `unavailable`. Of 338
reasoning records, 179 were so. The signature beside it is named in `dropped`.

## Kiro Crew

Kiro Crew runs every chat through `kiro-cli acp --agent-engine v3` when its `agent.acp_backend` is
`kas`, so its sessions are V3 sessions under the directory Crew works in. Measured with Crew 0.7.2:

| Crew does | In the store |
| --- | --- |
| a chat in the dashboard | one session over many prompts, in the directory the chat works in, by default Crew's own |
| `kirocrew chat -m` | a new session for each message |
| a background job | a session under `_bg` in Crew's workspace |
| a scheduled job | a session kept under `cron_<job id>` in Crew's workspace |
| a task's steps, `kirocrew run` | temporary sessions: 16 created and 14 removed in one run, lifetimes 1.4 to 358 seconds, median 68 |

A collector that reads every few minutes does not see most task steps: Crew frees the session and
the engine removes it before the next read. A session found and then removed keeps what was landed.
Kiro Crew issue [#18988](https://github.com/kirodotdev/KiroCrew/issues/18988) proposes a setting
that keeps these records for a while before Crew deletes them.

## What this adapter cannot supply

| Not in the files | Why |
| --- | --- |
| tokens | the engine records credits per turn in `usage_summary`, and no token counts, so no token metric is derived |
| model calls | no record names its request; see [above](#model-output-without-a-model-call) |
| the model per record | no record names the model that answered; the header names the one chosen |
| a permission decision as a step | `pending_interaction` and `interaction_resolved` land and are kept; the model has a step for them, and nothing draws it from Kiro yet |
| an interruption as a step | a turn the person cancelled ends with `stopReason` `cancelled`, which lands as a notice |
| a large tool output whole | the model is sent a preview, which is the result here; the rest is in `tool-outputs`, which is not read |
| a conversation over several sessions | Kiro Crew names no chain between the sessions of one chat |
| records the engine writes on recovery | they end their id in `-synthetic`; none was measured, and they land as what they are |
