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

### Goals

A **goal** is the targets that serve one result the person asked for. A conversation can hold one
goal or several, since a person often keeps working in one session on unrelated things. A goal has no
criteria and no score of its own. It groups targets for the report.

The evaluator says how each input relates to the inputs before it:

- `new` starts a goal.
- `continue` goes on with a goal without changing what it asks.
- `adjust` changes what a goal asks: it refines, steers, corrects, adds or cancels a target in it.
- `return` goes back to a goal that the input just before did not act on.
- `unclear` names no goal.
- `not_from_person` is an input the person did not write, as its actions say.

An input that asks for two unrelated things has one relation for each. A `continue`, `adjust` or
`return` names the earlier input of its goal that it follows. The statements given with `new` and
`adjust` are the goal's evolution: what the goal asked for after each input that set or changed it.

asz keeps a relation only when the record allows it. A relation to a goal must follow an earlier
input of that goal. A `new` must start a goal that no earlier input relates to. A `return` after an
input of the same goal is a `continue`. Anything else becomes `unclear`, the relation that claims
nothing. asz gives a new goal the next id, such as `g2`, in the order its first input came.

A target belongs to the goal of the input that created it, and to one goal only. A goal's main target
is the member whose result the goal is for. It is the only member, or the member the person's words
point to, which the evaluator must cite. Otherwise the main target is undetermined, as it becomes
when a goal whose only member was its main gains members and nothing cited says which is main. It is
never chosen by cost, recency or size. A main target cancelled or replaced by the end leaves the goal's
main undetermined in the report. The header of the targets stage counts the goals and the targets that
no goal holds.

### The person's goals

Grouping is the judge's unless a person gives their own. A person knows what they wanted from a
conversation, but not which of its inputs or steps each part took: an agent conversation is too long
to group by hand. So a goals file is a plain list of the person's goals, in their own words:

```yaml
goals:
  - Finish the post-vote steps of the 0.4.0 release
  - Remove the website pull request step from the post-vote script
```

asz gives them ids in order, `P1`, `P2` and so on. A file that lists none, or a goal that is empty,
longer than 2,000 bytes or given twice, is not read.

A regroup is made over a closed run, as a file of its own beside it, and the run is never changed.
Goals given with a new run are applied once it closes, never shown to the stages of the run: a
grouping made with the person's goals in view would measure nothing against them. Each regroup has a
version, names the goals file and its SHA-256, and keeps the file's text, so each change of the
prompts or the model can be measured against the same goals, even after the file changed.

Placing a target in a goal is a judgment of meaning, so one evaluator call makes it: the one
`-evaluator` names, or the plan's targets stage's. It reads the conversation as the run did, at its
head, through its window, with the calls priced at the rates of the run's ledger, and an input the
targets stage found was not the person's shows nothing. It is given the person's goals and the run's
targets, but not the judge's goals, so the comparison measures them. For each target it
names the person's goal the target serves, citing the person's input that shows it, or none. A target
placed in a goal with no input of the person that shows it, or in a goal the person did not give, is
unknown, and so is one the answer left out. A target in none of the goals is the evaluator's judgment,
which no record can show. For each goal it says whether the conversation worked on it, citing the
person's input that asked for it; a goal worked on with nothing that shows it is unknown.

asz then compares the two groupings, goal by goal:

| Class | When |
| --- | --- |
| `agree` | its targets are in one goal of the judge, which holds no other of the person's goals |
| `split` | its targets are in several goals of the judge |
| `merged` | its targets are in one goal of the judge that also holds another of the person's goals |
| `missed` | no target serves it, though the conversation worked on it, and every target was placed |
| `absent` | no target serves it, nothing in the conversation worked on it, and every target was placed: the evaluator's judgment, with how much of the judged work it was given (see [A claim of absence](#a-claim-of-absence)) |
| `unknown` | no target serves it, and the regroup cannot tell why: the answer left it out, a target put in it has nothing that shows it, a target was left out, or nothing shows it was worked on |

A target the judge put in no goal stands alone. The targets that serve none of the person's goals are
listed apart. A goal's main target is its only member still wanted, or the main target of a goal of
the judge that holds the same targets: the person's words that made it main were about those targets
alone. The goals change only how the report groups targets. A target's scores, and the run's, are the
same in both views. A run whose targets stage found no target is regrouped too, and a goal the
conversation worked on is then missed. A decision the answer leaves out is none it made: a target with
no goal given, or a goal with no answer to whether it was worked on, is not answered. Each regroup
reserves its version with its work directory, so two made at once never share one. A regroup whose
call failed is kept, failed, with what the call consumed, and the report follows the latest regroup
that compared anything.

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

### Reasons, observations and accounts

Each criterion carries a reason: why its state holds. When asz changes a state, the reason is asz's,
such as "nothing it cites shows it", and the evaluator's own reason is kept apart. A criterion can also
carry observations of the steps it cites. An observation is an excerpt of a step, what the record says
of the step, and what the evaluator reads in it. The excerpt must be in the step word for word, with
whitespace folded and a JSON escape of a line break read as one, and asz reads the step's record whole
when the document clipped it. An excerpt longer than 300 bytes is cut to its first 300. On a run of
2026-10-23, an evaluator quoted aligned output as one line, and gave excerpts of 318 and 313 bytes, the
second in 161 characters. asz reads what
the record says of the step itself: a tool step that `failed`, `returned` or gave `no_result`, an
`error` of the model API, or an interrupt. That is a fact. The interpretation is not: a query that ran
without error can report a test run that failed.

A target also carries an account of what was done for it, by what it was for. For a question, the
account says what was asked, what was checked or tried with what it found and how far that holds, what
was ruled out, and the answer. The answer must cite a reply of the agent, what was asked must be the
person's input, and each check must cite a step that shows what it found. For a change to another
system, the account lists each operation: the system, the operation, the tool or API it went through,
the request, the response, and what confirmed it later. The request must cite the tool step that sent
it, the response a step that returned, and the confirmation a step after the request that shows
something happened, or the person's input after the inputs that set the target. A call shows neither
that a change was accepted nor that it was completed, so a confirmation is kept only when something
later shows it. A part that nothing it cites shows is left out.

For changes to files, the account groups the target's file operations by what they were for: the
area, such as a package, the category, such as code, tests or documents, and what the group changed.
A group names operations by id, and only the target's own, each in one group: those of the calls the
target received, as the close gives calls to targets. A talk two targets share can hold calls only one
of them received, after an input that directed the other.

### The files a target changed

asz lists the file operations the record shows, never a claim of everything that changed. An
operation is a file that a workspace change record captured, or a file that the request of a tool
step names and no change record of that step names, as the request names it or within the record's
workspace. A capture can name some of a step's files and not others. A request shows an attempt, not a change, so such
an operation says whether its tool returned, failed or never returned. Which tools edit files, and
where their requests name the file, is the runtime's vocabulary, so each runtime's adapter reads its
own requests. In the 60 most recent Claude Code transcripts of one machine, every `Edit` (1,040)
and `Write` (666) named its file in `file_path`. In the 4 Kiro session files of one machine that held
`fs_write`, all 47 calls named theirs in `path`. LangSmith has no editing tools. A change record whose
file list was taken out to fit is one operation, `unlisted`: files changed, which are not named; the
request of its step still names its files.

The list is history, not a net diff. An edit repeated, two captures of one change and a later revert
are operations of their own. Operations take ids, such as `o3`, in the order they landed, so a run at
one head gives the same ids. The navigation's `operations` tool lists them.

### Deliveries and contradictions

A target's result can be delivered, taken back and delivered again. The evaluator lists these
**events** in recorded order: `delivered`, the result or one part of it reaching the person; `undone`,
a revert, a deletion, a rollback, or the agent withdrawing its answer; and `delivered_again`, a
corrected or redone result. A reply that only repeats a result already delivered, such as a closing
summary, is no event. On a run of 2026-10-23, an evaluator gave such a summary as a delivery again, and
a source's contradiction of the first answer then reached no score. Each event says what it concerns: an `answer`, a `revision` such as a commit or a
pull request, a `file`, or a `remote` change. asz gives each event an id, such as `t1.e2`, and keeps it
only when what it cites shows it. A reply shows an answer given or withdrawn. A tool step that
returned shows anything else. A request to revert is not a revert. The events must also come in
order: a delivery first, in one part or several, an undoing only after a delivery, after an undoing a
delivery again, and each event recorded after the one before it. On a run of 2026-10-23, an evaluator
gave one result as three deliveries: a script, the guide that describes it, and the reply that reported
both. asz places an event at the earliest step that shows it. A target with criteria met and no
delivery kept leaves the stage partial, since nothing a source reports of it could reach its score. On
the same run, an evaluator gave a code change no delivery.

A **contradiction** is evidence that a delivered result is wrong. It names the delivery event it
concerns. It disproves either the delivered conclusion itself, such as the answer or the cause named,
or some criteria, which it names. It must cite evidence beyond the agent's words: a tool result, an
error, or the person's input after the inputs that set the target. A contradiction stays tied to its
delivery. The deliveries standing at the end are every delivery except those a later delivery again
replaced. A delivery again names the earlier deliveries it corrects, such as one of two files, or none
when it replaces all of them. So when a corrected result is delivered again, the contradictions of the
earlier delivery remain in the history, and the parts it did not correct still stand. An undoing names
the criteria it took back, or none for the whole result, and the deliveries it took back, or none for
every one standing then. It applies at the end while one of those deliveries still stands, so
correcting another part does not clear it. One that names criteria, none of them the target's, or
deliveries, none of them earlier deliveries, is left out rather than read as the whole, and so is a
delivery again that names only what is no earlier delivery. The level is still derived from the criteria. Events and contradictions are kept
for the final statement.

### Rework

A **round of rework** is work the agent did again for a target. It has an earlier attempt, a trigger
that showed the attempt had to be done again, and the redo. The trigger is `person`, an input of the
person, including a correction typed while the agent was still working; `verification`, a check such
as a test or a build that failed; or `tool`, another tool result. The cause is `agent_error`,
`requirement_change` or `unknown`.

asz keeps a round only when the attempt, the trigger and the redo come in that order, each shown by
what it cites. The trigger can be the attempt's own step, as a command whose error shows it must be
run again. The attempt is the agent's work for the target, or the agent's work before the judged
window. The redo is the agent's work for the target, a sub-task it started included. A person's
trigger is an input of the person, and another trigger is a tool step that returned. Only the person
can change a requirement, so a round with that cause and another trigger has an unknown cause. A
round runs from its trigger to the last step of its redo, and asz gives it an id, such as `t1.r1`.
Rework never lowers the level of a target.

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

## Findings

A **finding** is a problem a stage observed: what went wrong, why, and what would have avoided it. It
is anchored at the step or input where the problem happened, and it cites what shows it. The
achievement stage records findings in a target's work. The efficiency stage records them for the work
it labels avoidable, each naming its target, as frames of their own. asz keeps a finding only when its
anchor is a step of the work it concerns: a step of the target's talks or of their sub-tasks, or, in
the achievement stage, a step before the judged window. A finding proposes. It never changes a state,
a level, a label or a cost.

## Judging against a source

A person often knows what the conversation should have found, such as the cause an incident report
names or the decision a ticket records. A run given such a source compares the conversation with it.
Before any evaluator runs, asz saves each source with a manifest: where it came from, when asz took
the copy, how it was fetched, its content type, size and SHA-256, and what the source says of its
revision, its publication and its author. A file and a plain http or https URL need no setting. A
source that needs a login goes through a configured command, given the source and never the
conversation. The copy is kept in the run's `work/sources/`, write-once, before any evaluator runs:
the manifest, the fetched bytes and the text an evaluator reads, which for a page is the text taken
out of it, with its own digest. So a run whose reference stage never runs still holds what it was given,
and a page's digest can be checked after the page changed. A copy is bounded, and saved cut when larger;
what a command writes is bounded too, and a file is read only when it is a regular file, since a named
pipe waits for a writer and a device never ends. A source that cannot be fetched is kept as failed, and
its comparison is unavailable, which is not the same as no contradiction found. A source is data to
judge by, never instructions. The time of the copy says when asz took it, never when what it says was
knowable.

A source can also be fetched by one tool of an MCP server, remote or local, which asz calls once and
offers nothing: no sampling, roots or elicitation, and the server's tools are never given to an
evaluator. asz speaks the protocol's revision `2026-07-28`, where each request carries its version, and
the revisions before it that begin with a handshake, `2025-11-25`, `2025-06-18`, `2025-03-26` and
`2024-11-05`; the first exchange finds which one a server speaks. The text the evaluators read is the
tool's text blocks joined by a blank line, or with none, its structured content as JSON. Blocks that are
not text, such as images, are left out, and so is structured content beside text; the manifest counts
both, so an evaluator and a reader know the copy is not all the server sent. A tool that reports a
failure leaves the source failed.

What is kept of a source is its provenance, never a credential. A URL is kept without its query and
fragment, and the manifest says one was there; a URL with a user or a password stops the run before it
writes anything. An MCP server is kept as its URL's scheme and host, or its program's name, and the
arguments as sent, a secret one by the name of the variable that held it. A fetch over http, through a
command or through an MCP server that fails is kept as a class, `timeout`, `transport`, `protocol`,
`server error`, `tool error` or `too large`, and a detail asz builds, with no URL the request used
beyond what is kept. Before anything of an MCP fetch is kept, asz
removes the secrets of the fetch from the result, every failure and the server's standard error: as
written, inside every JSON string literal however it is escaped, and in text escaped as JSON with no
quotes around it, to four levels of string inside string, a literal that holds one replaced whole. A
failure also clears the URLs the request used however they are spelled, and an http fetch the
credential of its proxy. Which blocks are text is read before anything is
removed, so a secret that matches a word of the protocol changes no decision.

The `reference` stage finds the conclusions each source holds, and how far each can be trusted. Each
conclusion is `final` or `intermediate`, `required` or `explanatory`, and names the inputs of the
conversation it is about. Its support is `saved` when the source holds the evidence, `linked` when it
only names or links it, such as a heap dump it mentions, and `asserted` when it is the author's word
alone. A conclusion the source corrects later names the correction in `superseded_by`, and changes no
score; the correction does. `conflicts` is for conclusions that disagree with neither correcting the
other. asz keeps the ids unique, a support of `saved` only with cited lines of the source, cited lines
within the source, `builds_on` and `superseded_by` free of cycles, and a correction from the same source.
Inputs its scope names that the conversation does not hold are kept apart as unbound. The stage is one
call however many sources there are, so these hold across all of them.

The `divergence` stage compares the conversation with each conclusion:

| Verdict | When |
| --- | --- |
| `reached` | the agent's reply, or a step of its work, reached it |
| `contradicted` | it shows a delivered result wrong: the delivered conclusion itself, or some criteria |
| `not_observed` | nothing reached or contradicted it |
| `unknown` | the evaluator could not tell |
| `not_applicable` | it is about something the conversation did not set out to do |

A contradiction names a target the conclusion is about, through an input of its scope that set or
changed the target, a delivery event of that target, and the criteria it shows not met, or the
delivered conclusion, which it cites. A source that says a delivered result was undone outside the
conversation, such as a release rolled back, gives a rollback of the same shape. The stage is told
which deliveries stand at the end, and asked to name one of those. What does not hold is
unknown. A verdict of absence, `not_observed`, rests on reading all of where the conclusion would show,
which the record cannot show, so it is marked as the evaluator's judgment, with how much of the judged
work the evaluator was given (see [A claim of absence](#a-claim-of-absence)).

A contradiction or a rollback of a delivery standing at the end applies to the final score as one the
conversation shows does: the delivered conclusion leaves 0, and criteria count as not met. A rollback
of a correct result makes the target `reverted`; it is `contradicted` only when evidence also shows it
wrong. When two conclusions about the same target disagree, neither applies, since the sources do not
agree. A conclusion about no input changes no score. A contradiction or a rollback that changes no
score is kept, and the report says why: its delivery no longer stands, or another conclusion about the
target disagrees.

The stage also records why the work went another way, at the step where it did, with a cause the
evidence supports: a misleading tool result, a result read wrong, a sub-task's untrue report, context
that never arrived, context that was there and not used, a question never asked
(`question_not_asked`), or an assumption never checked (`assumption_not_checked`). Missing context
needs evidence of what failed to arrive. A misleading sub-task needs one the sub-task stage found misled
the agent, started in the target's work before the step the finding is at, which the finding cites: a
step of it, or the step that started it. A question never asked and an assumption never checked rest on
absence, so they are the evaluator's judgment. For these two the answer gives `from`, the step where
the ambiguity or the assumption arose, and asz reports how much of the steps from it to the finding
the evaluator was given. Where the record supports none, the cause is unavailable.

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

## A claim of absence

Four claims say that something did not happen: a verdict of `not_observed`, the causes
`question_not_asked` and `assumption_not_checked`, and a person's goal found `absent`. Each rests on
reading all of where it would have shown, which the record cannot prove, so each stays the evaluator's
judgment. Beside it asz reports how much of that place the evaluator was given, counted in steps:

| Count | The steps |
| --- | --- |
| opened | the evaluator asked for the step itself, and the answer was not empty: `step` with its id and a text or a result, `record` with a part the step owns and a slice that is not empty, or `search` in it with a match |
| listed | a row the evaluator was given named the step: an outline line, a search match, an input row, a talk row's input or reply, a sub-task row's start or report, or the same rows in the prompt. A row read whole by its number, in slices, names the steps whose ids the slice holds |
| none | neither |
| unavailable | among them all, the steps some of whose evidence no one can be given: a part too large to land, cut, withheld, removed or dropped, or an image, in an input or in a tool's result |

The place is the judged work for `not_observed` and `absent`: the main talks a window judges with
every sub-task they started, or every talk and loose work for a run over the whole conversation. For
the two causes it is the span of one stream from `from` to the finding's step, in stream order, with
the sub-tasks a step of it started. A `from` in another stream, or after the finding's step, gives no
count, and the report says why.

The counts are facts about what the evaluator asked for and what it was answered, never about what it
understood. An answer counts once it was given: an API evaluator's when a later request carries it to
the model whole, and an agent CLI's when its navigation server wrote it. That server writes each
answer on its log with the name of the attempt, and a closing line with how many it wrote. It writes
the closing line when its input ends, or when the CLI ends it with an interrupt or a termination
signal, as Claude Code 2.1.312 does with SIGINT, after the answer being given is written and logged. A
log of another attempt, a line that is not one whole entry, or a closing line that is missing or gives
no count, as when the server was killed outright, makes the counts unavailable, never smaller.

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
  its own. It is given the goals found so far the same way, each with the last input related to it,
  and a relation of its own inputs can follow an input of an earlier call. asz gives a new target the
  next id in the order its first input came, numbers its criteria, and removes nothing: a criterion the person dropped is marked dropped and is never judged. The
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

### How each target ended

Each outcome has a **class**, most severe first:

| Class | When |
| --- | --- |
| `contradicted` | evidence shows a delivery standing at the end wrong, its conclusion or some of its criteria |
| `reverted` | an undoing applies: it took back a delivery that still stands at the end |
| `not_achieved`, `partly`, `achieved` | the level, as the criteria give it |
| `cannot_verify` | nothing shows the result either way |
| `cancelled`, `replaced` | the person no longer wanted the result; never scored |

Only the contradictions of the deliveries standing at the end apply: when a corrected result was
delivered again, the contradictions of the earlier delivery stay in the history. A correct result rolled
back is `reverted`; it is `contradicted` only when evidence also shows it wrong.
Rework shows beside the class, never as a class of its own.

Each target has two scores, from 0 to 1. The **record's score** is what the criteria give, counted as
the achievement score counts a target. The **final score** starts from the same criteria and applies
each contradiction of the deliveries standing, and their undoing, once. A contradiction of the delivered
conclusion leaves 0, and so does an undoing of the whole result. A contradiction of some criteria, or
an undoing of them, counts those criteria as not met, and the score is taken again. Rework never
lowers a score: it shows in what it cost and in the efficiency.

The outcome also says what the target's rounds of rework cost. A round holds the target's calls from
its trigger to the last step of its redo. The calls of every round, each counted once however many
rounds hold it, are priced as the target's cost is: by the label the efficiency or sub-task stage gave
each, with the target's share of a shared call and a sub-task's own label first. A round's cause does
not price it: work labeled a changed requirement stays the person's cost, and work no stage labeled
stays not reviewed. It lists the person's inputs that corrected the target, and the target's file
operations: those of the calls the target received, with its share when a call was shared. A step that
no model call asked for, as some runtimes record, belongs to the targets of its talk. The operations
no target's calls made are listed for the conversation.

asz records findings of its own, anchored where each problem happened: each criterion not met, at its
first cited evidence; each contradiction of a standing delivery, and an undoing, at the event; each
round of rework at its trigger; and each correction at the person's input, unless a round of rework
began there. A target cancelled or replaced keeps only the findings of its rework and corrections,
since what its result missed was no longer wanted.

A goal only groups targets. It has no outcome or score of its own: how each of its targets ended is
shown with the target, and grouping targets into goals never changes a target's score or the run's.

A run has two scores, which are never combined into one:

| Score | Is |
| --- | --- |
| achievement | the mean over the targets judged `achieved`, `partly` or `not_achieved`, counting `achieved` 1, `not_achieved` 0, and `partly` the share of its criteria met, each contradicted claim counting as one more not met. Cancelled and replaced targets, and targets that cannot be verified, are left out and counted apart. Beside it, `final` is the mean of the targets' final scores, over every target that has one |
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
| `targets` | the targets and goals the call was given, one row each, or one whole: a target's status, goal, talks and the inputs it cites, then its statement and each criterion; a goal's members, main target and the inputs that support it, then its statement and each step of its evolution; a long text in pieces |
| `changes`, `diff` | the workspace changes recorded per tool call, and one file's diff |
| `source` | the saved sources the call was given, a row each; with an id, that one's lines, numbered from 1, from a line on, a line over 2,000 bytes in parts numbered from 1 |
| `conclusions` | the conclusions a divergence call compares with the conversation, a row each, whole |
| `operations` | the file operations, in the order they landed: each one's id, step, talk, path and operation, and whether a change record captured it or a tool's request named it, with whether that tool returned |

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
is `unknown`. A relation the record does not allow becomes `unclear`, and a target whose goal cannot
be shown stays in no goal. A stage with anything left out or filled in is `partial`, and its note says what.

## What a run cannot know

- Whether a file changed, when no recorder captured the change. Workspace changes are recorded per tool
  call only as the [Claude Code plugin](../setup/claude-code-plugin.md) and the runtime record them.
  Without them, what changed is `not_captured`, never unchanged.
- What a call consumed, when the runtime recorded no usage for it.
- How long the agent was busy, as opposed to waiting.
- What a call cost, when its model or its price is unknown.
- Whether a judgment is right. A run says what its evaluator decided, on what it read. Agreement
  between evaluators, or with a person, is measured by comparing runs.
