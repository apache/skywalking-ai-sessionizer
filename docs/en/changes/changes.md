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
- Each target ends in a class, most severe first: contradicted, reverted, not achieved, partly,
  achieved, cannot be verified, or cancelled or replaced. It has a record's score and a final score
  that applies the contradictions and undoing of the deliveries standing at the end; what its rework
  cost, priced by how its calls were labeled; the person's corrections; and its file operations. asz records a finding where each
  problem happened, and each goal says its main target, its worst class, the problem that came first
  and its scores.
- The targets stage groups targets into goals, the targets that serve one result the person asked for,
  since a person often works on unrelated things in one session. It says how each input relates to the
  inputs before it: it starts, goes on with, adjusts or returns to a goal, or is unclear. A goal keeps
  how what it asked for changed, and its main target when the person's words show one.
- The achievement stage records each target's deliveries in order: delivered, undone and delivered
  again, each shown by the reply or the tool step that did it. A contradiction ties evidence that a
  delivered result is wrong to the delivery it concerns, and names the conclusion or the criteria it
  disproves.
- Each criterion says why its state holds, and what the steps it cites show: an excerpt exactly as
  recorded, the step's status as the record gives it, and the evaluator's reading. When asz changes a
  state, its own reason is shown and the evaluator's kept apart.
- Each target has an account of what was done for it: for a question, what was checked, what was ruled
  out and the answer; for a change to another system, each operation with its request, its response and
  what confirmed it.
- A run lists the file operations the record shows: the files workspace change records captured, and
  the files the requests of editing tools name that no record of their step names, read by each runtime's
  adapter. A target's account groups its operations by what they were for.
- The achievement and efficiency stages record findings: a problem anchored at the step where it
  happened, why, and what would have avoided it. A finding proposes and never changes a score.
- The achievement stage records each round of rework: an earlier attempt, what showed it had to be
  done again, the person, a check or a tool, and the redo, in that order, with the cause: the agent's
  error, a changed requirement, or unknown.
- `asz judge -reference SRC` compares a run with what a person concluded: a file, an http or https
  URL, or a source a configured command fetches, such as a ticket. Each source is saved with a
  manifest before any evaluator runs. A `reference` stage finds the conclusions it holds and how far
  each can be trusted, and a `divergence` stage says how the conversation compares with each: reached,
  contradicted, not observed, unknown or not applicable, with rollbacks the source reports and why the
  work went another way. What contradicts or rolls back a standing delivery applies to the final score.
- `asz judge -labels FILE` regroups a run by a person's goals, a plain list in their own words, and
  `-regroup N` regroups a closed run. One evaluator call places each target in the goal it serves, and
  asz compares the two groupings goal by goal: agree, split, merged, missed, absent or unknown. The
  regroup is a versioned file beside the run, which is never changed. `-show` follows the person's
  goals.
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

## Release

- The release guide no longer has steps for Scoop and winget. No version was submitted to either,
  and none is planned. `publish` no longer prints them as a next step, and `--remove-old` no longer
  waits for a Scoop bucket. A package manager the project is not in yet still needs the PMC's
  agreement first.
- The text `publish` writes for the GitHub release has each paragraph and list item of the
  changelog on one line. A GitHub release draws every line break as a break, so the wrapped lines
  of `changes.md` broke sentences in the middle: 662 times on the 0.6.0 release, and 351, 147 and 88
  times on 0.3.0, 0.4.0 and 0.5.0. The four release pages were written again the same way, and
  only their line breaks changed.
