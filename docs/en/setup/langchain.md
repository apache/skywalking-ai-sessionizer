# LangChain and LangGraph

asz assembles conversations from an agent built on LangChain or LangGraph
without changing the application. It takes four environment variables.

The reason it is that small: `langsmith`, the tracing client, is a dependency
of `langchain-core`. Every LangChain application already carries it. Pointing
it at asz is the whole integration.

## Turn the receiver on

In `asz.yaml`:

```yaml
adapters:
  - name: langsmith-ingest
    enabled: true
    listen: 127.0.0.1:1985
```

Then `asz collect` (or `asz server`, which serves the page beside it):

```text
receiver    : langsmith-ingest on 127.0.0.1:1985; point LANGSMITH_ENDPOINT at it; any key accepted
source   : received on 127.0.0.1:1985 (every 10m)
```

## Point the application at it

```sh
export LANGSMITH_TRACING=true
export LANGSMITH_ENDPOINT=http://127.0.0.1:1985
export LANGSMITH_API_KEY=asz
export LANGSMITH_PROJECT=my-agent
```

`LANGSMITH_API_KEY` must be set because the client insists on sending one. Its
value does not matter unless the receiver is configured with a `token`.

Run the application. Within one collector period its conversations are in the
storage root:

```sh
asz conversation ls-my-agent-thread-1-9718de8d3d72
```

## Give each conversation a thread

A conversation is a thread, and the thread is supplied by the application. In
LangGraph that is the checkpointer's thread id:

```python
agent.invoke(
    {"messages": [{"role": "user", "content": "..."}]},
    config={"configurable": {"thread_id": "the-conversation"}},
)
```

Without one, asz has no conversation to assemble. It lands what arrived under a
session whose name says so, and invents nothing.

In plain LangChain, or with the client's own `@traceable`, pass the key as
metadata instead:

```python
@traceable(metadata={"thread_id": "the-conversation"})
def answer(question): ...
```

## What owns a conversation

A thread key alone is not an identity. Two applications can both use the
project `production` and the thread `123`, and merging them would put two
people's histories in one conversation. So the receiver names the dimensions
that own one:

```yaml
    scope:
      - project
      - thread
```

`project` is `LANGSMITH_PROJECT`. Taking it out merges every project that
shares a thread key, which is sometimes what you want and is never assumed.

`thread_keys` names the metadata keys that may carry the thread, in the order
they are tried. The default is `thread_id`, then `session_id`, then
`conversation_id`.

## Reaching it from another machine

`listen` binds locally by default, because what arrives is whole prompts and
whole tool output. An application in another container needs an address it can
reach and a token:

```yaml
    listen: 0.0.0.0:1985
    token: a-shared-secret
```

```sh
export LANGSMITH_API_KEY=a-shared-secret
```

## What arrives, and when

The client sends in the background, in batches. A run is sent when it starts
and again when it finishes, so asz sees work while it is still running: a turn
with a slow tool appears before the tool returns, and a process that dies
mid-turn still leaves what it had reported. That evidence lands as it arrived,
and the conversation says the work was unfinished rather than pretending it
never happened.

The prompt that opens a turn, a tool command and a tool result are landed whole,
as they were sent. Nothing is sampled and nothing is redacted, which is the same
treatment a Claude Code transcript gets.

What is not landed is the history that repeats. Every model call carries the
whole conversation again in its inputs, and a call's record keeps what the model
said rather than what it was told, so a landed conversation is 7 to 8% of what
arrived. See [the adapter page](../adapters/langsmith.md) for what that costs.

## What it cannot do

- **A sub-agent cannot have its own conversation.** Measured: an agent invoked
  with its own `thread_id` reports the parent's on every one of its runs,
  because the surrounding run context wins. Its work is part of the
  conversation that delegated to it, in an execution stream of its own.
- **A tool that calls a model gets a stream of its own, but is not a
  sub-agent.** Its prompt does not continue the caller's, so it is kept apart
  for the continuity check. It is counted as a tool step, not as an agent.
- **A failed tool does not always say which call it answered.** When a tool
  raises, its output is empty and the call id is nowhere in its run. asz reads
  it from the model call that asked for the tool, and when that arrived in a
  different batch, the result lands joined to nothing rather than to a guess.
- **There is no context reset.** LangChain has no compaction, so a conversation
  is one epoch however long it runs.

## Seeing it work without an application

The repository carries a demo application that needs no API key and no network:

```sh
make langchain-capture
```

It runs sixteen shapes of conversation against a stand-in model and writes what
the client sent into `tests/apps/langchain/testdata`, with `MEASUREMENTS.md`
beside it.
