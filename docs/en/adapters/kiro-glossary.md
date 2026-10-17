# Kiro Glossary

What Kiro's V3 agent engine calls the things the
[project glossary](../concepts-and-designs/glossary.md) names, and where each one appears in the
files it writes.

The dialect is **`kiro/1`**. That names the vocabulary, not the adapter: `kiro-local` says how
records were acquired, the dialect says how they are read.

Two words here mean something other than what a reader might expect. Kiro's **execution** is one
turn, which is a Run here. And Kiro writes a subagent's records in a file of their own, a
**sub-execution**, which is a child stream here.

The tables below are copied from what `asz glossary kiro/1` prints. The command reads the adapter's
own declarations, so where this page and the command differ, the command is right.

```sh
asz glossary kiro/1                 this table
asz conversation ID                 talk · run · tool
asz conversation -terms native ID   talk · executionId · tool_call and tool_result
asz conversation -terms both ID     run  (executionId)
```

**61 terms: 24 that Kiro names, 37 this project derives.**

---

## Named by Kiro

Each row says where the thing is recorded.

| Model | Kiro | Where | Note |
| --- | --- | --- | --- |
| `agent.call` | `sub_agent_start` | messages.jsonl | no tool call names the spawn; the record is the call |
| `agent.output` | `sub_agent_complete` | messages.jsonl | the response the parent received; the child's own last record is in its file |
| `child` | `subSessionId` | sub_agent_start and sub_agent_complete | also the name of the child's file under sub-executions |
| `context.injection` | `agent_note, session_start, steering_inclusion` | messages.jsonl | and a user record with syntheticUserMessageReason, the engine's text around a steering message |
| `control.notice` | `turn_end, ContextualHookInvoked` | messages.jsonl | a turn_end whose stopReason is not end_turn; also a record with no type, or a type this adapter does not describe |
| `conversation` | `id` | session.json | one session is one conversation; Kiro Crew names no chain between the sessions of one chat |
| `epoch.boundary` | `tombstone` | messages.jsonl | kind summarization or checkpoint_revert |
| `epoch.summary` | `assistant, operationType Summary` | messages.jsonl | nothing names the tombstone it follows |
| `error.api` | `session_metadata, key displayError` | messages.jsonl |  |
| `external` | `user` | messages.jsonl |  |
| `id` | `id` | every record | followed by # and the first 12 hex digits of the record's SHA-256; the engine gives one id to an approval it asks twice |
| `message.assistant` | `assistant, operationType Say` | messages.jsonl |  |
| `message.external` | `user` | messages.jsonl | also a message sent during a turn, source steer; Kiro Crew writes its own prompts here too |
| `redacted` | `reasoningRedactedContent` | a Reasoning record |  |
| `result_of` | `toolCallId` | tool_result | subSessionId for a subagent's end |
| `run` | `executionId` | turn_start and the records of the turn | a prompt carries none; it belongs to the first turn named after it in the file |
| `session` | `session` | sessions/<workspace key>/sess_<id> |  |
| `starts` | `subSessionId` | sub_agent_start |  |
| `stream` | `sub-execution` | sub-executions/<subSessionId>.jsonl | the main stream is messages.jsonl |
| `thinking` | `assistant, operationType Reasoning` | messages.jsonl | the text ... means the engine kept no reasoning |
| `time` | `timestamp` | every record | not in line order; line order is the order |
| `tool` | `tool_call and tool_result` | messages.jsonl and sub-executions | joined by toolCallId |
| `turn.duration` | `usage_summary` | messages.jsonl | elapsedTime, with the turn's credits; no token counts |
| `unavailable` | `...` | the content of a Reasoning record |  |

---

## Derived by this project

Nothing in the files carries these. They are what assembly works out from the evidence above, and
saying so is more useful than a plausible name that would send a reader looking for a field that is
not there.

| Model | Kiro | Where | Note |
| --- | --- | --- | --- |
| `agent.launch_ack` | — | derived here | no equivalent: nothing acknowledges a subagent separately |
| `available` | — | derived here | the engine keeps text and tool results whole, apart from large tool output |
| `batch` | — | derived here | no equivalent: subagents started together share only their turn |
| `call` | — | derived here | no equivalent: a turn lists its request ids in usage_summary, and no record names one |
| `cancels` | — | derived here | no equivalent |
| `conflict` | — | derived here | derived |
| `continues` | — | derived here | no equivalent: a tombstone's effectiveFromMessageId names where the dropped records begin, not the last one kept |
| `control.command` | — | derived here | no equivalent: a slash command leaves no record of its own |
| `control.interrupt` | — | derived here | not read: a turn the person cancelled ends with stopReason cancelled, which lands as a notice |
| `control.permission` | — | derived here | not read: pending_interaction and interaction_resolved are landed and are no step |
| `ends_with` | — | derived here | derived from a child's last response |
| `epoch` | — | derived here | the span between two tombstones |
| `exact_ambiguous` | — | derived here | derived |
| `exact_unique` | — | derived here | one toolCallId or subSessionId on both records |
| `follows` | — | derived here | derived from line order |
| `hash_only` | — | derived here | no equivalent |
| `in_segment` | — | derived here | derived |
| `input_of` | — | derived here | no equivalent |
| `llm.call` | — | derived here | no equivalent: the engine records what the model said and not which request said it |
| `message.synthetic` | — | derived here | not read: records the engine writes on recovery end their id in -synthetic, are a tool result, a child's end or a turn's end, and none was measured |
| `model` | — | derived here | not read: no record says which model answered; session.json names the model chosen |
| `notification` | — | derived here | no equivalent |
| `omitted` | — | derived here | no equivalent |
| `parent` | — | derived here | no equivalent: a record names no record before it |
| `removed` | — | derived here | no equivalent: asz removes the payloads of a conversation idle longer than storage.retention.metadata_only_after from its own root |
| `reports` | — | derived here | no equivalent |
| `retries` | — | derived here | no equivalent |
| `runtime.notification` | — | derived here | no equivalent: a subagent's end is the result of its call |
| `segment` | — | derived here | an activity window |
| `size_only` | — | derived here | no equivalent |
| `strong_inference` | — | derived here | derived |
| `summarizes` | — | derived here | not read: nothing joins a summary to its tombstone |
| `talk` | — | derived here | one readable interaction: a prompt and the turn it opened |
| `too_large` | — | derived here | no equivalent: asz omits a payload whose record alone is larger than a landed file's byte budget |
| `trigger` | — | derived here | derived: a user record that is neither a steering note nor written by the engine |
| `truncated` | — | derived here | not read: the model is sent a preview of a large tool output, and that preview is the result here; the whole output is in tool-outputs, which is not collected |
| `unknown` | — | derived here | derived |
| `unresolved` | — | derived here | derived |
| `weak_inference` | — | derived here | derived |
