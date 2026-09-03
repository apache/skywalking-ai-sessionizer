# Claude Code Glossary

What Claude Code calls the things the [project glossary](../concepts-and-designs/glossary.md) names,
and where each one appears in its data.

The dialect is **`claude-code/1`**. That names the vocabulary, not the adapter: `claude-code-local`
says how records were acquired, the dialect says how they are read. A push receiver for the same
runtime would be a different adapter and the same dialect.

This table is generated from the code — `asz glossary` prints it — so it cannot drift from the
mapping it describes. A test fails if the project's vocabulary grows a name with no entry here.

```sh
asz glossary                        this table
asz conversation ID                 talk · run · llm.call · tool
asz conversation -terms native ID   talk · promptId · message.id · tool_use + tool_result
asz conversation -terms both ID     run  (promptId)
```

**59 terms: 36 that Claude Code names, 23 this project derives.** That ratio is worth watching. If
everything had a native name the model would be a rename of one product rather than a model of any;
if nothing did, it would have no evidence under it.

---

## Named by Claude Code

Each row is a field the adapter actually reads. A rename in Claude Code changes this table and the
extraction together, because they sit in the same file.

| model | Claude Code | where | note |
| --- | --- | --- | --- |
| `agent.call` | `Agent, Skill or Workflow tool_use` | an assistant record |  |
| `agent.launch_ack` | `toolUseResult.status == "async_launched"` | a tool result | an acknowledgement, not a result |
| `batch` | `toolUseResult.runId` | a workflow launch result | also the name of the directory the batch's records are filed under |
| `call` | `message.id` | record | not requestId, which a client-fabricated record can reuse |
| `cancels` | `toolUseResult.interrupted` | a tool result |  |
| `child` | `toolUseResult.agentId` | a launch result, a journal record |  |
| `context.injection` | `attachment` | an attachment record |  |
| `continues` | `logicalParentUuid` | a compact_boundary record | the only link back; a boundary's own parentUuid is null |
| `control.command` | `system/local_command` | a system record |  |
| `control.interrupt` | `toolUseResult.interrupted` | a tool result |  |
| `control.permission` | `permission-mode` | a record type |  |
| `epoch.boundary` | `system/compact_boundary` | a system record |  |
| `epoch.summary` | `isCompactSummary` | a user record |  |
| `error.api` | `system/api_error` | a system record |  |
| `external` | `origin.kind == "human"` | record | also a run whose records state no origin at all - a locally typed command |
| `follows` | `logicalParentUuid` | a compact_boundary record |  |
| `id` | `uuid` | record |  |
| `llm.call` | `message.id` | assistant records sharing one | one call is several records; 2.09 on average |
| `message.assistant` | `text block` | an assistant record |  |
| `message.external` | `origin.kind == "human"` | a user record | also a queued_command attachment whose commandMode is "prompt" |
| `message.synthetic` | `message.model == "<synthetic>"` | an assistant record | the client wrote it; no model produced it |
| `notification` | `origin.kind == "task-notification"` | record |  |
| `parent` | `parentUuid` | record |  |
| `redacted` | `redacted_thinking` | a content block |  |
| `reports` | `<task-notification>` | a user record's content |  |
| `result_of` | `tool_use_id` | a tool_result block | also stated redundantly as sourceToolAssistantUUID |
| `run` | `promptId` | record | one loop per distinct promptId |
| `runtime.notification` | `<task-notification>` | a user record's content |  |
| `session` | `sessionId` | record, and the transcript filename |  |
| `starts` | `toolUseResult.agentId, or a run journal` | a launch result |  |
| `stream` | `agentId` | a child record | the parent lineage is the session transcript itself |
| `summarizes` | `parentUuid` | an isCompactSummary record |  |
| `thinking` | `thinking block` | an assistant record |  |
| `time` | `timestamp` | record |  |
| `tool` | `tool_use + tool_result` | two records | one step here, two blocks there |
| `trigger` | `origin.kind` | record, and attachment.origin.kind |  |

## Derived by this project

Claude Code has no word for any of these. That is an answer, not a gap: saying so is more useful
than a plausible word that would send a reader looking for a field that does not exist.

| model | what it is |
| --- | --- |
| `agent.output` | the last model response in a child stream |
| `available` | the bytes are in a landed record |
| `conflict` | sources disagree |
| `conversation` | supplied, never inferred; defaults to the session id |
| `ends_with` | the last model response in a child stream |
| `epoch` | the span between two compact_boundary records |
| `exact_ambiguous` | an identifier matched several; nothing chose one |
| `exact_unique` | an identifier matched exactly one candidate |
| `hash_only` | only a digest of the bytes was kept |
| `in_segment` | an activity window chosen for commit |
| `input_of` | unavailable; the serialized provider request is not written locally |
| `omitted` | deliberately not carried |
| `retries` | unavailable; retry identity is not recorded |
| `segment` | an activity window chosen for commit |
| `size_only` | only the length was kept |
| `strong_inference` | no identifier, but a well-founded structural argument |
| `talk` | one readable interaction; derived from where a run was triggered |
| `truncated` | part of the bytes, with the full length reported |
| `turn.duration` | unavailable on the claude-vscode entrypoint |
| `unavailable` | the runtime never wrote it, or the source was pruned |
| `unknown` | no record of the run states a trigger |
| `unresolved` | no usable evidence |
| `weak_inference` | plausible only |

---

## What Claude Code records and the model does not carry

The mapping runs both ways, and this direction matters for anyone going back to the source.

| field | why it is not carried |
| --- | --- |
| `requestId` | identifies a provider attempt, which nothing resolves against. A client-fabricated record can reuse a real one, so grouping by it would put invented content inside a real call. It is the join key to captured provider bodies and will return when those do. |
| `agentId` | equals the stream name for a child stream; carrying it would store the same value twice. |
| `sessionId` | the landed file already says which session it belongs to. |
| `isSidechain` | absent on 28.7% of parent-lineage records, so it cannot say which stream a record belongs to. The file can. |
| `cwd`, `gitBranch`, `slug`, `version`, `userType` | environment, not conversation. Still in the landed record for anyone who wants it. |
| `thinking.signature` | 13.3% of the corpus, wrapping 0.1 MB of reasoning text. A provider verifies it; a reader cannot read it. |

## What Claude Code does not supply at all

Reported `unavailable` rather than approximated. A second adapter might do better on any of these.

| | |
| --- | --- |
| tool execution timing | only `WebFetch.durationMs`, `WebSearch.durationSeconds` and `Agent.totalDurationMs`. Everything else has none, and a timestamp gap includes model and harness latency. |
| turn duration | absent on the `claude-vscode` entrypoint — 13 of 15 sessions sampled. |
| a title or summary | for a session, a talk, or anything else. |
| retry identity | a retried attempt is not linked to the one it replaced. |
| the serialized provider request | never written locally, so model-context membership is a cross-check on the reconstruction rather than a statement about the wire. |
| its own record schema | not published, and it changed during this project. |
