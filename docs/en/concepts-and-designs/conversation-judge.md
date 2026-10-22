# Conversation Judge

A judge run measures one conversation. For each target the person set, it says whether the target
was achieved, what achieving it cost in tokens, money and time, and how much of that went to work
that could have been avoided. Wrong steps matter because they cost. The run does not try to find
the one step where things went wrong.

A run never compares conversations. Comparing runs is left to a later mode, run on request, that will
read only the results runs have saved.

## A run

A run measures a conversation at one head of its [round chain](conversation-assembly.md): the current
head, or an earlier round asked for. Every answer inside the run is about that head, however many
rounds are published while it runs, and a record or a change record that landed after it is not shown. A run
happens only when a person runs [`asz judge`](../setup/judge.md). Nothing in `collect`, `server` or
`push` starts one. A conversation can be judged again later, at a newer head, or again at the same
head, as often as anyone asks. Each run is complete on its own, and runs are never merged.

A run can judge the whole conversation or a window of it. A window takes whole main talks: those that
start in a time range, or those from one talk id to another. Each selected talk comes with every
sub-task it started, however long that ran, because work outlives the talk that started it: in
0947c9ef, 39 of 50 child agents ended after their talk did, and 14 ran past the start of a later main
talk. Cutting by each call's time would price work whose start the run cannot see, or show evidence
whose cost it leaves out. The main talks before the window are context: an evaluator can read them,
marked `before`, and they are neither judged nor priced. Nothing recorded after the selected work ends
is shown, not even what an earlier talk's sub-task recorded later, and a step that started before then
keeps only the records written by then: a tool or an agent call whose result came later shows no
result, a model call still running counts what it had read and its output is unknown, with nothing of
what came with its end and the model its kept records name, and a change or
execution record written later is left out with the step's link to it. A talk that ran past the end
ends there, with its reply and counts as of then. Achievement is judged as of that end.
Loose work and streams no step can be shown to have started belong to runs over the whole
conversation.

A run has six stages, in this order:

| Stage | Made by | What it holds |
| --- | --- | --- |
| `ledger` | asz, with no model | what every model call consumed, and what it cost where a price applies |
| `targets` | an evaluator | the targets the person set, and what each input did to them |
| `achievement` | an evaluator | whether each target was achieved, and on what evidence |
| `subtasks` | an evaluator | how each sub-task went: the work the agent gave to a sub-agent or a helper stream |
| `efficiency` | an evaluator | what each piece of the agent's own work was |
| `close` | asz, with no model | each target's outcome and cost, and the cost of each kind of work |

Each stage can be given to a different evaluator. Each is written once, as one
[Session Judgment](../formats/session-judgment.md) file. A stage that fails is written as failed, with
the reason, and the run still closes.

## Two layers

The numbers and the judgments come from different places, and never mix.

- **Computed.** asz counts every model call, prices it, and adds up every share. The same head and
  the same price file always give the same numbers.
- **Judged.** An evaluator names targets, decides achievement, and labels work. It never gives a
  number. asz prices every label from the calls it counted.

So a judgment can be wrong, but the cost it points at cannot be made up.

## Targets

A **target** is an outcome the person asked for, with criteria that could be checked. "Fix the login
timeout and keep the public API" is a target. "Authentication" is a topic. A question is a target
too, and its criterion is a correct answer.

The evaluator reads every input from outside the agent, in order, and says what each did to the
targets: `create`, `refine`, `steer`, `accept`, `correct`, `cancel`, `none`, `not_from_person` or
`unknown`. One input can act on more than one target. A target ends `open`, `closed`, `cancelled`,
or `replaced` by another when the person changed what they wanted.

A target lists the [talks](unified-conversation-model.md#hierarchy) whose work serves it. The talk is
the unit, because assembly already decides which steps, child agents and file changes belong to a
talk, from the joins the runtime recorded. The evaluator only decides which talks serve which
target. A talk can serve two targets when its inputs act on both. Of the 5,437 talks in 63 local
conversations, child agents' talks included, 649 held more than one input.

## Achievement

Each criterion of a target has an id, such as `t2.c1`, which asz gives when the targets stage is
accepted. The evaluator gives every criterion a state, `met`, `not_met`, `unknown` or `not_captured`,
with citations. asz derives the target's level from those states, so a level never disagrees with its
own criteria:

| Level | When |
| --- | --- |
| `achieved` | every criterion is `met` |
| `partly` | some criteria are `met`, and others are not |
| `not_achieved` | no criterion is `met`, and at least one is `not_met` |
| `cannot_verify` | anything else, a target with no criterion included |

Evidence that does not hold changes the decision, not only a note. A criterion the evaluator left out
is `unknown`. A `met` or `not_met` holds only when what it cites shows it: a tool step that returned a
result, a failed one included, an API error or an interrupt, or the person's own later input: an
input of the main stream, never one the agent gave a sub-agent nor one the targets stage found came
from no person. The input
that set the target shows what was asked, never that it was done, and a tool call whose result never
came shows nothing. The agent's replies, reasoning, model calls and a sub-agent's report show only what
was said, with one exception: when an answer is what was asked for, the reply that delivered it shows
the answer was delivered. That holds for the whole target, with the basis `delivered_answer`, or for one
criterion the evaluator marks `delivered`, such as an explanation asked for within a change. In
a0fc872b, "explain findings 2 and 3 before changing them" was met by the reply and by nothing else. A
file, a document or a change is never shown by a reply. A talk id cites nothing in particular. Whether a
delivered answer is true is a claim: the evaluator lists each statement of the answer the record can
check as a claim of its own, and a target with a contradicted claim is not achieved, whatever its
criteria say. asz cannot read meaning, so which criteria ask for an answer is the evaluator's word.
In round three of the local evaluation, both judges took an explanation that put a code path on the
wrong side for met, and its reviewers found it; the prompt now asks for those statements to be checked.

The outcome says what it rests on: `person_accepted`, `observed_result`, `delivered_answer`,
`agent_claim_only` or `no_evidence`. A basis must be shown by what is cited: the person's input after
every input that set the target for `person_accepted`, a tool step for `observed_result`, a reply for
`delivered_answer`. One that is not
is weakened to `agent_claim_only` or `no_evidence`. The agent's own claims about the outcome, such as
"the tests pass", are checked against the tool results: `supported`, `contradicted`, `no_evidence` or
`not_captured`. A claim only the agent's words support or contradict is `no_evidence`.

## Sub-tasks

A **sub-task** is work the agent gave to another stream: a child agent, or a helper stream such as a
summariser. It is what that stream was asked, the work it did in its own stream, and what it reported
back. Every stream a step started is a sub-task of that step, followed through the `starts` relations
as deep as the agents went, so a sub-agent's own sub-agents are sub-tasks of it. Over 63 local
conversations, 2,789 child streams: 28 were started by another child, all in one conversation and two
levels deep, each with a `starts` relation. A LangChain conversation had an auxiliary `summarise`
stream started by a step of the main talk; it is a sub-task as much as a child is. A stream no single
step can be shown to have started stays unattributed, with the reason: no starting step (5 of the
2,789), several, or a parent that is unattributed itself. It can still be read and judged on its own.

A sub-task belongs to the targets of the talk that started it, and its calls take the place of its
starting step when that talk is cut at its inputs. It is judged once, whatever targets that is:

| Decision | Values | Says |
| --- | --- | --- |
| `outcome` | the levels of an achievement | whether it did what it was asked, by its own steps |
| `report` | `accurate`, `inaccurate`, `unverifiable` | whether what it reported back is true to its own work |
| `use` | `used`, `misled`, `unused`, `unknown` | whether the agent that asked used the result; `misled` when it acted on a report found `inaccurate` |
| `dispatch` | `reasonable`, `redundant`, `misdirected`, `unknown` | whether giving the work away was reasonable when it was given; `misdirected` when the task itself was wrong: its scope, its instructions or its command |
| `work` | the labels of efficiency, `misdirected_delegation` among them and `misled_by_subtask` not | what its own cost was |

Sub-tasks with the same decisions, such as the agents of one workflow, can share one decision, and asz
writes one per sub-task. Each is checked on its own: an outcome or a report holds for a sub-task only
when a citation points into its own talks, what its own steps did. The result of the step that started
it is its report back, a claim, and does not count. So one citation cannot vouch for sub-tasks nobody
read. Its report is its own last reply, so the steps that show its outcome are also what its report is
checked against. Whether it was used is shown by what the agent that asked did next: a step of its
own, a tool, an agent call, a reply or the final output a sub-agent passes back, in its stream after
the starting step, never an input, a runtime notice or context it was sent. A decision with nothing valid behind it falls back to the value that claims nothing. Its work label
cites a step of its own work, or the work is not reviewed; an evaluator that could not read enough of
it says `not_reviewed` itself. A nested sub-task is never covered by its parent's decision.

What the sub-agent says in its own messages is a claim about its work, as its report back is: an
outcome or a report rests on a tool step of its own and the result it returned. In round four of the
local evaluation, a cheaper model judging the sub-tasks of the 221ac729 window cited the sub-agents'
own messages, and 115 of its decisions were weakened, where the stronger model's were not.

A sub-task's work goes wrong across the boundary in two ways, and each is priced to whoever caused it.
A task can be wrong when it is given: too wide or too narrow, against what the person asked, a command
or its arguments built wrong, or instructions the agent had already replaced. A `misdirected` dispatch
cites the step that gave the task, whose input is the task, and what shows it wrong besides: a tool
result, an error, or the person's words. The sub-task then did what it was told, so its work is
`misdirected_delegation`, the main agent's cost. In 221ac729, the main agent rewrote a design brief
for the person's new requirement while a design agent still worked on the old one; the old design went
unused, and that was the main agent's doing. Another step of the same agent giving a task, as when it
corrected the task or gave it again, also shows the first one wrong; the agent saying in a message that
the task was wrong is its own account, and shows nothing. A report can be untrue, and the
agent that asked can act on it: `misled` holds only on a report found `inaccurate` and a later step of
that agent. The work that followed, labeled `misled_by_subtask` from the step that acted on the report,
is the sub-task's cost when its task was right and its own work
went wrong, and the cost of the agent that gave the task when the task was wrong. When the record does
not show whether the task was right, its cause is unknown: charged to neither. That work begins after the
report came back: with the earliest runtime notice that the sub-task ended, or, for one that ran in the
foreground, with the result of the call that started it, which the record marks as the sub-task's own. A
background launch's acknowledgement is not a report, and with neither, nothing shows the report came
back and no work is charged to it. In the talk the report came back to, the work begins at a later step;
in another talk, after it in the order the records landed. Order is never taken from times, which can go
back or be missing.

The agent that gave a task is the main agent for a sub-task a main talk started, and the sub-agent above
it for a nested one. A sub-task given work already done, `redundant_delegation`, also did what it was
told, so that cost too is the giver's. Each sub-task row the navigation gives carries the sub-task
stage's decision once that stage ran, so the efficiency stage reads what was decided of every sub-task,
not only the misleading ones its prompt lists.

## Efficiency

Each talk of a target, or each part of a talk from a step on, gets one label. A part runs from its
first step to the next part, in the order the work happened. It takes in the model call that asked for
its first step, since a runtime's view puts a tool inside that call, and a call that asked for the first
steps of several parts belongs to the first of them. A child agent's calls stay with the part its own
starting step is in. When the run has a sub-task stage, a
label covers only the talk's own stream: each sub-task's calls are priced by its own `work`, and a
sub-task left unjudged is not reviewed, as is every sub-task when that stage fails. Without one, a sub-task's calls take the label of the part its
starting step is in.

| Label | Avoidable | Meaning |
| --- | --- | --- |
| `useful` | no | moved the target forward |
| `exploration` | no | looking around a careful engineer would also do, even when it found nothing |
| `verification` | no | checking the work |
| `avoidable_attempt` | yes | an approach that failed for a reason visible at the time, or a detour |
| `repeated` | yes | the same action again with nothing new learned, while the earlier result was still in the agent's context |
| `redundant_delegation` | yes | a sub-task whose work duplicated work already done or under way when it was started |
| `repair_own_mistake` | yes | fixing a defect in the agent's own work after it said the work was done, whoever found it, or a mistake that went against something visible when it was made; a defect its own checks found before it delivered is normal work |
| `rework_requirement_change` | no | work done again because the person changed what they wanted; the person's cost |
| `misdirected_delegation` | yes | a sub-task's own work when the task it was given was wrong; the main agent's cost. asz gives it to a sub-task whose dispatch is `misdirected` |
| `misled_by_subtask` | yes | the asking agent's work done on a sub-task's untrue report, naming the sub-task; a label of the main talks, for a sub-task the sub-task stage found `misled` it |
| `not_reviewed` | no | work no evaluator labeled; asz gives it, never an evaluator |

The sub-task stage and the efficiency stage are given one definition of the labels, so a sub-agent's
work and the agent's own are labeled by the same rules. Each talk and sub-task row points where
avoidable work is most often found: the tool steps that failed, and the tool steps that ran the same
input again. In round two of the local evaluation, the reviewers found the judges labeling long talks
useful as a whole and missing the repairs and the repeated failing commands inside them; the rows now
say where to look, and the evaluator labels such a part on its own.

The evaluator is told to judge with what was known at the moment, not with hindsight. Every label cites
a step of the talk it labels, the one read to decide it, and a label other than the first three cites
the step that shows why. A talk id is not a step. A label with no step of its talk is left out, and its
cost counts as not reviewed rather than reviewed or avoidable.

## Cost

The ledger counts each model call once, under one owner: the talk that contains it, or the calls of
a stream that no talk contains. The owners add up to the conversation exactly. A main talk's cost
with its child agents adds the child streams that talk alone started, followed through the
`starts` relations as deep as they go: a child agent can start child agents of its own. Over 63 local
conversations, 28 of 2,789 child streams were started by another child, two levels deep at most, and
each had a `starts` relation. A child stream no single talk can be shown to have started is
unattributed: one no call names, or one two talks claim. Five of the 2,789 had no `starts` relation.

A target's cost is the cost of its talks. A talk serving several targets is cut at its inputs: each
call goes to the targets the last input before it directed work to, by creating, refining, steering or
correcting them. Accepting, cancelling and thanks direct nothing, so the work after them goes on for
what came before. A child agent's calls take the place of the step that started it. Only calls that
one input directed to several targets, or that come before any directing input, are split equally,
and the close file says so with each share. In 772204d1 one talk held a question and then a change the
person typed while the agent worked: 4 calls, 245k tokens, came before the change and 14 calls, 1.12M
tokens, after it. An equal split had charged the question 682k. Talks no target claims are
unassigned.

## What an evaluator reads

An evaluator reads the conversation only through the navigation, from an overview down to single
steps and records, and each answer is at most 16 KiB. One evaluator call has a reading budget,
`judge.read_bytes`, counted in bytes of answers. asz writes the answers, so it counts them exactly; the
tokens they become are the model's, which asz cannot see while the call runs. An answer that would read
past the budget is refused whole, never cut, and the evaluator is told to answer with what it read and
to say `unknown` for the rest. An agent CLI's navigation server ends after three such answers. The
prompt states the budget and every answer says what is left, so an evaluator can keep enough for the
decisions still open. The prompt also gives the card, so no stage begins by asking for it, and the
sub-task and efficiency stages are given the steps the achievement stage cited for each target the call
is given. Searching the
conversation covers each step's first 2,000 bytes; searching one named step covers its whole recorded
text, each match saying its part and byte offset.

The budget is there for cost as much as for context. An agent CLI sends its whole context again on
each turn, so the tokens a call consumes grow with the square of what it reads. Replaying every
navigation call of 13 runs over 8 local conversations: a stage read a median of 22 KB for targets,
70 KB for achievement and 39 KB for efficiency, and up to 446 KB on a conversation of 44 talks, where
one achievement call of 49 reads consumed 3.3M input-side tokens. Each stage records what it read, so
the budget can be set from what runs actually read.

## What judging consumes

What the evaluators consume is counted, never estimated: every number is what the provider reported.
An API evaluator's every response carries its usage. claude, run with its event stream, says each
request's input side when the request starts and its output when it ends, and its own cost; asz counts
each request as it ends. claude also runs a small model of its own beside the one asked for, which only
its totals include, and asz counts every model. codex says only what a whole call consumed, when it
ends, so for codex the requests one by one are `unavailable`. A stage records how many requests its
calls made, the largest input side of one, and the window of the model when the evaluator said it, so
whether a request came near the model's window is read, not guessed. Each attempt keeps its requests,
one per line. A request whose usage the provider never said, as one whose answer was lost on the way,
is counted apart: it may have been billed, so it is never taken for zero, and it never hides what the
other requests said. Judging DeepSeek through Bedrock, 4 of the 19 calls of one stage lost a response.
A claude call stopped before its result counts the requests its stream showed ending, and one more
apart: the one running, or one that may have been on its way before the stream showed it.

A stage of a large conversation takes minutes, so a running stage says every `judge.progress` what it
has consumed so far: the calls ended and running, the requests and tokens counted, the input and output
tokens per minute, and what the running calls read. When a stage ends it says its totals and its rate,
and when the run ends it says what all its evaluator stages consumed, over how long, and per minute.
The close file keeps that time, so `asz judge -show` gives the rate of a run read later.

## One call or several

A stage gives its evaluator the rows it answers for in the prompt, as the tools show them, so the
evaluator does not page through them: the inputs for the targets stage, the talks of each target, the
sub-tasks, the talks to label. When those rows fit `judge.single_bytes`, one call does the stage. When
they do not, the stage is cut into calls of at most `judge.chunk_bytes` each, with `judge.mode` `auto`;
`single` and `stream` force one way. Each call is also told which of the rows it reads are its own and
which are context. One item can be larger than a chunk, such as a target with many talks: its rows past
`judge.chunk_bytes` are named by count, and the tools list them.

- **Targets** is a scan in order. Each call answers for its own inputs and is given the catalogue the
  calls before it left. It may change a target found earlier, by its id, or set a new one, under a key of
  its own. asz gives a new target the next id in the order its first input came, numbers its criteria,
  and removes nothing: a criterion the person dropped is marked dropped and is never judged. The
  catalogue takes at most `judge.state_bytes` of the prompt: past it, the open targets stay whole and
  the others are shortened, then all are, then only their ids and status remain, as many as fit with a
  line saying how many more, and the `targets` tool gives any of them whole. The targets a later stage's
  call is given, and the steps the achievement stage cited for them, are bounded the same way. A call that fails twice fails the stage, since a later call would build on a
  catalogue missing its changes, and the stage counts the inputs it never read.
- **Achievement** is one call per target, even when every target would fit one, unless `single` is
  asked for: each target then has the reading budget to itself. In round two of the local evaluation,
  six targets of one conversation in one call read 117 KB of a 128 KiB budget and left three of them
  uninvestigated.
- **Sub-tasks** and **efficiency** are calls side by side, up to `judge.concurrency` at once, merged in
  order. Each call is also cut so the outlines of what it labels fit three quarters of its reading
  budget, since an evaluator that cannot open the work it labels labels it unread: in round two,
  efficiency over 40 talks of one conversation in one call left 80% of the cost unreviewed.

A call that gives nothing that can be read is tried once more, in a directory of its own, and what the
failed attempt consumed still counts. What a call that failed twice was to judge stays in the stage's
coverage, not judged, even when every call failed. A call is given only its own targets: those it
judges, those claiming a talk it labels, or those whose talks started a sub-task it judges. A stage is
written once, after every call has ended. Its header says how it ran: the mode, how many calls, how
many attempts, and the largest prompt one call was sent.

## The final statement

The close file gives each target an outcome: its level, the basis, how many criteria were met and how
many are unknown, what the target cost, the part labeled avoidable, the part of that sub-agents caused,
the part whose cause the record does not show, the part no label covers, and two times. Each avoidable
cost names its cause: the main agent, a sub-task, or unknown. The observed time is
the union of the spans of its talks and their child agents. The avoidable time is the union of the
spans of the main stream's avoidable work: a run of avoidable calls lasts from its first call to the
talk's next call, or to the talk's end. Child agents' work can run beside the main stream, so it adds
cost, not time. Waits inside a talk are inside these spans and cannot be told apart.

A run has two scores, which are never combined into one:

| Score | Is |
| --- | --- |
| achievement | the mean over the targets judged `achieved`, `partly` or `not_achieved`, counting `achieved` 1, `not_achieved` 0, and `partly` the share of its criteria met, each contradicted claim counting as one more not met. Cancelled and replaced targets, and targets that cannot be verified, are left out and counted apart |
| efficiency | 1 less the avoidable part of the reviewed cost. The share of the targets' cost a label covers is given beside it |

Beside them, `delivered` is the part of the targets' cost that went to targets achieved. The scores
are taken from each call's exact share, so a token split between three targets counts whole, though
each target's rounded amount shows none of it. Costs are
compared in money when every call of the targets was priced, and in tokens otherwise, since a token
read from the cache costs a tenth of one read fresh. The close file also adds up what the run's
evaluators consumed, priced when the price file lists their models, so the cost of judging can be set
against the cost of the conversation.

What a call consumed is read as the runtime recorded it, in four types: input, output, cache read and
cache write. The types mean different things in different dialects. Claude Code counts them apart.
LangChain counts cached tokens inside its input, so the ledger takes them out once. A call that
recorded no usage, or a count below zero, has unknown usage, and it is counted as unknown, never as
zero.

A call that never finished is counted by what it read. Claude Code writes a response's usage onto each
fragment of it as the response streams: what the call read and wrote to the cache is known when the
response starts, and its output grows until the response ends. Over the local conversations, 56,919
finished calls had several fragments, and the input side was the same on every fragment of every one.
Inside a child agent's transcript the final fragment is often never written: in one conversation, 698
of 1,309 child-agent calls. Such a call counts its input side, and its output is unknown. A sub-agent's
calls are the conversation's cost as much as the main agent's are, and what they read is most of it.

Money needs a [price file](../setup/judge.md#prices) and the model each call ran on. A call with no
model, or no price for its model, is unpriced, and its tokens still count. Files landed before asz
kept each call's model have none.

Time is what was observed. A target's span is the union of the time spans of its talks with their
child agents, so two talks at once count once. It is not the time the agent was busy: a talk can wait
inside itself, and permission waits are not recorded.

## How an evaluator reads

A conversation can be far larger than any model's context. Over 63 local conversations, the largest
held 922 talks, 49,827 steps and 87 MB of recorded text and results. Even each talk's input and reply,
clipped to 200 bytes each, came to about 300 KB for that one. So an evaluator never gets the
conversation at once.
It reads through navigation, from the overview to the details:

| Tool | Gives |
| --- | --- |
| `card` | the conversation at a glance: counts, time range, what its calls consumed, what was and was not captured |
| `inputs` | every input from outside the agent, in order, each clipped to 500 bytes, with the last thing the agent said before it |
| `talks` | the main talks: input label, reply, tool and child counts, failures, and cost with children |
| `subtasks` | the sub-tasks, one level at a time: what each was asked and reported, its failed steps, its talks, how many sub-tasks are under it, its cost with all of them, and, once the sub-task stage ran, its decision |
| `talk` | one talk as an outline, one line per step. `only` keeps the tools, the tools that failed, or the messages |
| `step` | one step whole |
| `record` | one landed record, in 8 KiB slices; with `part`, that part's text from a byte offset |
| `search` | text in the steps of the conversation, or of some talks: what the document holds of each step, its text and its result up to 2,000 bytes each. A search that finds nothing does not show the text is absent. With `node`, one step's whole recorded text: only the part its reference names, since one record can hold several tools' results, each match with its part and byte offset, and at most 50 matches, after which it says how to go on with `skip` |
| `targets` | the targets the call was given, one row each, or one whole: its status, talks and the inputs it cites, then its statement and each criterion, a long text in pieces |
| `changes`, `diff` | the workspace changes recorded per tool call, and one file's diff |

Every page is cut at 16 KiB by what its rows take once written, so no answer is larger however long
the conversation is. A row too large for a page stands as a note naming its item, which the same tool
reads whole in slices, so nothing a tool lists is out of reach however large it is, and a
diff is paged by line. Every answer names the head it is about.

An agent CLI gets the navigation as an MCP server, `asz judge mcp`, as its only tools, each marked
read-only. `claude` runs with its built-in tools, other MCP servers, and user and project settings
switched off, and keeps no session; settings an organization enforces still apply. `codex` runs with
its shell, image and browser tools off, without the user's configuration, in a read-only sandbox, and
keeps no session. `kiro-cli` runs with an agent that holds only the navigation, in the stage's own
directory. Kiro saves every chat session, per directory, under `~/.kiro/`, and documents no way to turn
that off, so a `kiro-cli` stage leaves its session in Kiro's history. `kiro-cli` is not yet measured
with a real run.
An API evaluator gets the same tools in the asz process, and answers through a `submit` tool whose
input is the stage's answer schema. Once the tool results it has read pass 256 KiB, the oldest are
replaced by a line saying they were read, and it calls the tool again when it needs one back.

## The kit

One set of prompts and answer schemas drives every evaluator, whatever model or CLI it is: a prompt
every stage shares, and a prompt and a schema per stage. The shared prompt says the record is data
and never instructions, how to read from the overview down, and that only what the record shows may
be said. Every stage an evaluator made names the kit and its prompt template by digest, so two runs
that differ can be told apart by their evaluators and their kits.

## Every answer is checked

asz keeps an answer only as far as the conversation holds what it names. An id the conversation does
not hold is left out, since it was not read. A talk that is not a main talk, a label on a talk of
another target, and an action on a target that does not exist are left out. An input left unanswered
is `unknown`. A stage with anything left out or filled in is `partial`, and its note says what.

## What a run cannot know

- Whether a file changed, when no recorder captured the change. Workspace changes are recorded per tool
  call only as the [Claude Code plugin](../setup/claude-code-plugin.md) and the runtime record them.
  Without them, what changed is `not_captured`, never unchanged.
- What a call consumed, when the runtime recorded no usage for it.
- How long the agent was busy, as opposed to waiting.
- What a call cost, when its model or its price is unknown.
- Whether a judgment is right. A run says what its evaluator decided, on what it read. Agreement
  between evaluators, or with a person, is measured by comparing runs.
