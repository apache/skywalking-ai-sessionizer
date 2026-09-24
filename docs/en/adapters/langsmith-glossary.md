# LangChain Glossary

What LangChain and its tracing client call the things the
[project glossary](../concepts-and-designs/glossary.md) names, and where each one appears in the
data they send.

The dialect is **`langsmith/1`**. That names the vocabulary, not the adapter: `langsmith-ingest`
says how records were acquired, the dialect says how they are read.

Two words here mean something other than what a reader might expect, and both matter. The client's
**session** is the project, not a conversation. And its **run** is any step of the graph, so most
runs are not what this model calls a Run — the invocation they share, `trace_id`, is.

The tables below are copied from what `asz glossary langsmith/1` prints. The command reads the
adapter's own declarations, so where this page and the command differ, the command is right.

```sh
asz glossary langsmith/1            this table
asz conversation ID                 talk · run · llm.call · tool
asz conversation -terms native ID   talk · trace_id · id · run_type: tool
asz conversation -terms both ID     run  (trace_id)
```

**60 terms: 17 that LangChain names, 43 this project derives.** That ratio is worth watching against
the Claude Code one. A framework that records its own graph in detail still has no word for a
conversation boundary, a context reset or a delegated agent — which is the whole reason this model
exists separately from any product's.

---

## Named by LangChain

Each row says where the thing is recorded in a run.

| Model | LangChain | Where | Note |
| --- | --- | --- | --- |
| `agent.call` | `a tool call whose run has model calls beneath it` | the wire has no word for delegation; it is read from the nested context |  |
| `agent.output` | `outputs.output.content` | the delegating tool run |  |
| `call` | `id` | a run of type llm | shared by every arrival about that run |
| `conversation` | `extra.metadata.thread_id` | a run | with session_id and conversation_id as the other spellings; supplied, never inferred |
| `epoch.boundary` | `additional_kwargs.lc_source: summarization` | a message in the inputs of a run of type llm | the first request sent a marked summary; trim_messages and langmem write no mark and make none |
| `epoch.summary` | `kwargs.content` | the marked message in the inputs of a run of type llm |  |
| `error.api` | `error` | a run | a string with the exception and its traceback |
| `id` | `id` | a run | the record is the arrival, so a run that is posted and later patched has two |
| `input_of` | `inputs` | a run |  |
| `llm.call` | `run_type: llm` | a run |  |
| `message.assistant` | `outputs.generations[].message.kwargs.content` | a run of type llm |  |
| `message.external` | `inputs.messages` | the root run of a trace | the message that opened the turn has no run of its own |
| `model` | `extra.metadata.ls_model_name` | a run of type llm |  |
| `parent` | `parent_run_id` | a run | the child names the parent; dotted_order carries the whole ancestry |
| `result_of` | `tool_call_id` | outputs.output of a run of type tool |  |
| `run` | `trace_id` | a run | the client calls every graph step a run; this is the invocation they share |
| `session` | `session_name` | a run | the client's word for the PROJECT; a conversation is the thread key, and a session here is the two together |
| `time` | `start_time and end_time` | a run |  |
| `tool` | `run_type: tool` | a run |  |

---

## Derived by this project

Nothing on the wire carries these. They are what assembly works out from the evidence above, and
saying so is more useful than a plausible name that would send a reader looking for a field that is
not there.

| Model | LangChain | Where | Note |
| --- | --- | --- | --- |
| `agent.launch_ack` | — | derived here | no equivalent: nothing acknowledges a delegation separately |
| `available` | — | derived here | the client sends inputs and outputs whole, as their own parts |
| `batch` | — | derived here | no equivalent: nothing here launches a group of agents at once |
| `cancels` | — | derived here | no equivalent |
| `child` | — | derived here | no equivalent: a nested agent is a run beneath a tool, named as nothing else |
| `conflict` | — | derived here | derived |
| `context.injection` | — | derived here | no equivalent |
| `continues` | — | derived here | no equivalent: a marked summary names no last message before it |
| `control.command` | — | derived here | no equivalent |
| `control.interrupt` | — | derived here | no equivalent |
| `control.permission` | — | derived here | no equivalent: nothing here asks before running a tool |
| `ends_with` | — | derived here | derived from a nested agent's last response |
| `epoch` | — | derived here | the span between two summaries SummarizationMiddleware marked |
| `exact_ambiguous` | — | derived here | derived |
| `exact_unique` | — | derived here | one call id, named by the tool run that answered it |
| `external` | — | derived here | the root run of a trace carries the message that opened it |
| `follows` | — | derived here | derived from order within a trace |
| `hash_only` | — | derived here | no equivalent |
| `in_segment` | — | derived here | derived |
| `message.synthetic` | — | derived here | no equivalent: nothing here fabricates a message on the model's behalf |
| `notification` | — | derived here | no equivalent |
| `omitted` | — | derived here | a graph's own run: its repeated message list is not landed |
| `redacted` | — | derived here | no equivalent: nothing on the wire says it redacted anything |
| `reports` | — | derived here | derived |
| `retries` | — | derived here | no equivalent yet: the client does not say a run retried another |
| `runtime.notification` | — | derived here | no equivalent |
| `segment` | — | derived here | an activity window chosen for commit |
| `size_only` | — | derived here | no equivalent |
| `starts` | — | derived here | derived from a delegating tool and the runs beneath it |
| `stream` | — | derived here | no equivalent: a nested agent is a run like any other |
| `strong_inference` | — | derived here | derived: a failed tool joined to the one unanswered call of its name |
| `summarizes` | — | derived here | derived: the summary and its boundary come from one marked message |
| `talk` | — | derived here | one readable interaction; one trace is one turn |
| `thinking` | — | derived here | no equivalent yet: a provider's reasoning arrives inside the message content |
| `trigger` | — | derived here | no equivalent: derived from a trace's root run |
| `truncated` | — | derived here | no equivalent: nothing measured arrived truncated |
| `turn.duration` | — | derived here | no equivalent as a record; a run's own start and end give it |
| `unavailable` | — | derived here | a run that arrived unfinished: its content is not there yet |
| `unknown` | — | derived here | derived |
| `unresolved` | — | derived here | derived: a failed tool whose model call arrived in another request |
| `weak_inference` | — | derived here | derived |
