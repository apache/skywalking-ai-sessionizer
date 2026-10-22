# Changes in 0.7.0

> In development, not yet released. `tools/release/release.sh prepare 0.7.0` removes this note.

## Conversation judge

- `asz judge` measures one conversation at its current head: the targets the person set, whether each
  was achieved, what each cost in tokens, money and time, and how much of that went to avoidable work.
  It runs only when a person runs it, at a newer head or the same one as often as asked, and each run is
  kept. See [Conversation Judge](../concepts-and-designs/conversation-judge.md) and
  [Judge a Conversation](../setup/judge.md).
- A run has six stages: the ledger asz computes, then targets, achievement, sub-tasks and efficiency,
  each given to its own evaluator, then the close file that prices every decision from the ledger. An evaluator
  names and labels and never gives a number.
- Evaluators are the agent CLIs `claude`, `codex` and `kiro-cli`, run with their own tools switched
  off and the conversation as their only tools, or a model API asz calls itself: Anthropic's Messages
  API, any OpenAI chat completions endpoint, or Amazon Bedrock's Converse API. One kit of prompts and
  answer schemas drives them all, and every answer is checked against the conversation before it is
  kept.
- Evaluators read through a read-only navigation, from an overview down to one record, every answer cut
  at 16 KiB and pinned to the run's head. An agent CLI gets it as an MCP server, `asz judge mcp`.
- Each stage is one [Session Judgment](../formats/session-judgment.md) file, written once in the
  session's `judgments/` directory. A closed run is sent over OTLP, one record per stage file. See
  [Judge runs](../formats/otlp.md#judge-runs).
- `asz judge -round N` judges a conversation as it was at round N of its chain.
- `asz judge -from T -to T` and `-from-id ID -to-id ID` judge a window: whole main talks, each with every
  sub-task it started. The talks before the window are context, and nothing after it is shown.
- A run ends with a final statement: each target's level, basis, criteria met, cost, avoidable cost and
  time, and two scores, achievement and efficiency, with what the judging itself consumed.
- asz derives a target's level from its criteria, which carry ids. Evidence that does not hold weakens
  the decision: a criterion with no valid citation, or resting only on the agent's own words, is
  unknown, and an avoidable label with no citation is left out.
- A sub-task is the work the agent gave to a sub-agent or a helper stream, nested as deep as the agents
  went. Each is judged once: its outcome, whether its report back is true to its own work, whether it
  was used, whether giving it away was reasonable, and what its own cost was.
- Each evaluator call has a reading budget, `judge.read_bytes`, counted in bytes of the answers the
  navigation gives it. An answer that would read past it is refused whole, and the stage records what
  was read. Inputs say how many images came with them, which the view's text leaves out.
- A stage gives its evaluator the rows it answers for in the prompt, and is cut into calls when they do
  not fit: the targets stage as a scan that carries the catalogue, achievement one call per target, and
  sub-tasks and efficiency side by side. A call is given only its own targets, and the targets found so
  far take at most `judge.state_bytes` of a scan call's prompt. A call is tried once more when it gives
  nothing readable, and each stage records how it ran. `asz judge -mode` chooses.
- What judging consumes is counted per model request as the provider reported it: claude through its
  event stream, an API from each response; codex says one total per call. A running stage prints what
  it has consumed every `judge.progress`, and its totals when it ends.
- A sub-task's task can be found `misdirected`, its scope, instructions or command wrong when given, and
  the asking agent `misled` by a report found untrue. Each avoidable cost names whose doing it was: the
  main agent's, for a wrong task and what followed from it, or the sub-task's, when a right task's own
  work went wrong. The final statement says what sub-tasks caused.
- A Bedrock evaluator can use a Bedrock API key, sent as a bearer token, in place of AWS credentials.
- A talk serving several targets is cut at its inputs: each call goes to the targets the last input
  before it directed work to.
- `asz measure` prints what a conversation's model calls consumed, per talk with the child agents it
  started, at any depth, and what they cost where an optional price file prices them. It calls no model.
- A Claude Code call that never finished counts what it read, and its output is unknown. Claude Code
  writes the input side of a response when the response starts: over the local conversations, 56,919
  finished calls with several fragments had the same input side on every fragment. Inside a child
  agent's transcript the final fragment is often missing; in one conversation, 698 of 1,309 child-agent
  calls, whose cache reads alone added 36% to the conversation's.
- The asz.view document carries each model call's `model`, read from the same record as its usage.
