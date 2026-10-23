# Session Judgment

Session Judgment is the record of a [judge run](../concepts-and-designs/conversation-judge.md): one
file per stage of the run, written once. A file has the extension `.sj`, and the schema is `sj/1`.
Package `pkg/sessionjudge` defines it. It is public: the exporter reads it to send a run, and a
receiver reads it to store and list one, so nothing in it depends on how a run is made.

## A stage file

One stage file is one file of JSON lines, framed as a [round](session-flow.md#a-round) is. Every line
carries `t`, its frame type. The first line is the `header`, then any number of body frames, then
one `commit`.

```json
{"t":"header","schema":"sj/1","conversation":"7722…","session":"7722…","run":1,"stage":"achievement",
 "head":{"round":1,"digest":"41c2…"},"parser":"v1","policy":"v1+idle=10m0s",
 "builds_on":[{"stage":"ledger","digest":"a3f1…"},{"stage":"targets","digest":"be46…"}],
 "evaluator":{"name":"claude","kind":"cli","program":"claude","model":"claude-fable-5-1"},
 "kit":{"version":"5b0e…","prompt":"9c41…"},
 "started":"2026-10-20T08:16:30Z","finished":"2026-10-20T08:18:42Z","status":"complete",
 "coverage":{"targets":3,"targets_judged":3},"usage":{"in":258,"out":7345,"cache_read":279836,"cache_write":63445}}
{"t":"achievement","target":"t1","level":"achieved","basis":"person_accepted","criteria":[{"id":"t1.c1","text":"…","state":"met","cite":[…]}],"claims":[…],"note":"…","cite":[{"node":"input/1/59","ref":{"seq":1,"row":59}}]}
{"t":"commit","digest":"6ad7…","counts":{"achievement":3}}
```

### Header

| Field | Meaning |
| --- | --- |
| `conversation`, `session` | the conversation judged, and the session its rounds were assembled from |
| `window` | the part of the conversation the run judges, absent for all of it: `from` and `to` (RFC 3339), or `from_talk` and `to_talk`; `talks`, the main talks selected; `before`, how many earlier talks were context; `end`, the last record time of the selected work, in unix milliseconds |
| `run` | counts the runs of one conversation from 1. Two runs at the same head are two runs |
| `stage` | `ledger`, `targets`, `achievement`, `subtasks`, `efficiency`, `reference`, `divergence` or `close`; or `regroup` (see [A regroup](#a-regroup)) |
| `head` | the `round` and `digest` of the chain the run measured. Every citation addresses that fold |
| `parser`, `policy` | the chain's interpretation, from its rounds |
| `builds_on` | the earlier stages of the run this one read, each by `stage` and `digest` |
| `evaluator` | `name` in the configuration; `kind`, one of `computed`, `api` and `cli`; `program`, such as `asz`, `claude`, `codex`, `kiro-cli`, `anthropic`, `openai` or `bedrock`; `model` and `version` when known |
| `kit` | the kit that drove the stage: `version`, the digest of every kit file, and `prompt`, the digest of the stage's prompt template, the shared prompt and the stage's own. What the run appended to it, the head and the targets, is not in that digest. Absent on a computed stage |
| `started`, `finished` | when the stage ran, RFC 3339 in UTC. They are the evaluation's own times, never the conversation's |
| `status` | `complete`; `partial`, when the stage covered less than all or asz left something out of its answer; or `failed`, when nothing could be accepted |
| `note` | why a stage is partial or failed |
| `coverage` | what the stage covered, each count absent where it does not apply: `inputs` and `inputs_read`, `targets` and `targets_judged`, `goals` and `unplaced`, the targets no goal holds, `subtasks` and `subtasks_judged`, `talks` and `talks_labeled`, `calls` and `calls_without_usage`. A talk of a target counts as labeled only when every call of it that served the target has a label. A failed stage keeps it, with what no call answered not judged |
| `requests` | the stage's model requests, as the provider counted them: `count` and `largest`, the largest input side of one request, when the evaluator said what each consumed; `whole_only` when it gave one total per call instead, as codex does; `context_window`, the window of the model that did the work, when the evaluator said; `unknown`, the requests whose usage was never said, as one whose answer was lost on the way, which `usage` does not hold |
| `mode`, `chunks`, `attempts` | how an evaluator stage ran: `single`, one call, or `stream`, cut into `chunks` calls; `attempts` counts the calls made, retries included |
| `prompt_bytes` | the largest prompt one call of the stage was sent, in bytes |
| `reading` | what the evaluator read through the navigation in the stage: `calls`, `bytes` of answers, the `budget` it had, and how many answers were `refused` because the budget ran out |
| `usage`, `credits` | what the evaluator itself consumed, when it said, every model it used counted. Absent means unknown, never zero |
| `reported_cost_usd` | what the evaluator itself said the stage cost, in US dollars, when it said, as claude does. It is kept apart from any price asz applies |
| `summary` | on the close file only: what a list of runs shows. `targets` and the count at each level, `usage` of the conversation with `unknown` and `output_unknown`, the calls it leaves out, `avoidable` usage, the sum of the targets' own, `avoidable_by_subtasks`, the part sub-agents caused, `avoidable_cause_unknown`, the part whose cause the record does not show, and `money`, `currency` and `unpriced` when priced; `score`, with `final`, the mean of the targets' final scores; `goals`, each goal's `goal`, `main`, `worst` class, `first` finding, `score_main` and `score_mean`; `unassigned_operations`, the operations no target's calls made; and `judging`, what the run's evaluators consumed as an amount, absent when one of them did not say, its `unknown` counting the requests whose usage was never said; its `unpriced` counts the stages with a model that had no price; `judging_reported_usd`, what the evaluators said the run cost, present only when every stage that ran said; `judging_ms`, the time from the first evaluator stage's start to the last one's end |
| `score` | `achievement` and `judged`, the targets it is the mean over; `final`, the mean of the targets' final scores; `efficiency`, `reviewed` and `delivered`, each a fraction; `unit`, `money` or `tokens`. A number is absent when nothing could give it |

### Body frames

Which frames a file holds depends on its stage.

| Stage | Frames |
| --- | --- |
| `ledger` | `price` for every rate applied, then `cost`: the conversation, each main talk with its children, the main stream's calls outside any talk, and the unattributed calls |
| `targets` | `input` for every input from outside the agent, in order, then `target`, then `goal` |
| `achievement` | `achievement`, one per target judged |
| `subtasks` | `subtask`, one per sub-task judged |
| `efficiency` | `label`, one per talk or part of a talk, then `finding`, one per finding |
| `reference` | `source` for every source the run was given, saved or not, each followed by its `source_text` frames, then `conclusion`, one per conclusion found |
| `divergence` | `divergence`, one per conclusion, then `rollback`, one per rollback a source reports, then `finding`, one per finding |
| `close` | `stage` for every stage file of the run, `outcome` and `cost` for every target, `cost` of the unassigned and unattributed calls, `cost` of every kind of work, an `operation` for every file operation of the conversation, which outcomes and accounts name by id, and a `finding` for each problem asz records itself, its `source` `asz` |

| Frame | Fields |
| --- | --- |
| `price` | `model` as a call recorded it, `key` the price file entry it matched, `from`, `currency`, `per_million` with `in`, `out`, `cache_read` and `cache_write` |
| `cost` | `scope`, `id`, `target`, `work`, `share`, `calls`, `usage`, `unknown` (calls with unknown usage), `output_unknown` (calls that never finished: what they read is in `usage`, their output is not), `money`, `currency`, `unpriced` (calls with usage and no price), `from` and `to` (unix milliseconds), `children` (calls of child streams included) |
| `input` | `node`, `talk`, `actions`, each a `target` and an `action`; `relations`, at least one, each a `kind` and, where it applies, the `goal`, the earlier input it `follows`, the `request` of a mixed input it concerns, and the goal's `statement` after a `new` or `adjust` |
| `goal` | `id`, such as `g1`; `statement`; `members`, the targets in it by id; `main`, the member the goal is for, absent when undetermined, with `main_reason` and `main_cite`; `evolution`, each an `input` and the goal's `statement` after it, in order; `cite` |
| `target` | `id`, `statement`, `criteria`, each an `id` such as `t1.c1`, a `text`, and `dropped` when the person dropped it; `status`, `replaced_by`, `talks`, `cite` |
| `achievement` | `target`, `level` (derived by asz from the criteria), `basis`, `criteria`, each the criterion's `id`, its `text`, a `state` and its `cite`, a delivered answer counted as such, its `reason`, and `given_reason`, the evaluator's own when asz changed the state, and its `observations`, each a `node`, its `ref`, an `excerpt`, the `status` the record gives the step, and the evaluator's `interpretation`; `claims`, each a `text`, a `state` and its `cite`; `events`, in recorded order, each an `id` such as `t1.e1`, a `kind`, the `artifact` it concerns, for an undoing the `criteria` it took back, none for the whole result, and the deliveries it `undoes`, none for every one standing then, for a delivery again the earlier deliveries it `replaces`, none for every one, a `text` and its `cite`; `contradictions`, each the `event` it concerns, `conclusion` when it disproves the delivered conclusion itself or else the `criteria` it shows not met, a `text` and its `cite`; `rework`, each round an `id` such as `t1.r1`, the `attempt`, the `trigger` and its `trigger_cite`, the `cause`, the `redo`, `from` and `to`, the steps that bound it, `finished` and `note`; `account`, absent when none: `answer`, with what was `asked`, what was `checked` and `ruled_out`, each a `what`, what it `found`, its `limits` and its `cite`, and what was `answered`; `files`, each group's `area`, `category`, `summary` and the ids of its `operations`; and `remote`, each operation's `system`, `operation`, `via`, `request`, `response` and `confirmed`. A thing said is a `text` and its `cite`; `findings`, each as a `finding` frame without its `target`; `note`, `cite` |
| `operation` | `id`, such as `o3`, in the order the operations landed; `step`, absent for a change no step joins; `record` and `workspace` for a change record; `path`; `operation`, for a change record `create`, `modify`, `delete`, `type_change` or `unlisted` when its file list was taken out, and for a request `write` or `edit`; `source`, `change_record` or `tool_request`; `status` for a request, `returned`, `failed` or `no_result`; `attribution`; `added` and `removed`; `ref` |
| `source` | `id`, such as `R1`; `source`, as given; `captured`, when asz took the copy; `method`, `file`, `http` or `command`; `outcome`, `saved` or `failed`, with `error`; `content_type`, `bytes`, `sha256`, `cut` when it was larger than a copy may be; `revision`, `published` and `author`, as the source says; `text`, the `bytes` and `sha256` of the text taken out of a page; `lines` |
| `source_text` | `source`, `from`, the line it starts at, counting from 1, and `text`, at most 16 KiB of UTF-8 text, cut only between characters. A frame starts where a line starts, except inside a line longer than 16 KiB, which is cut across frames that each name that line in `from`; its last piece can share a frame with the lines after it. The frames of a source in order are its text. The bound is on the text, so a frame written out, with its escapes, can be larger |
| `conclusion` | `id`, `source`, `kind` (`final` or `intermediate`), `statement`, `expectation` (`required` or `explanatory`), `scope`, the inputs it is about, `context`, each a `source`, its first and last `lines` and a `note`; `builds_on`, `support` (`saved`, `linked` or `asserted`), `author`, `revision`, `environment`, `conflicts`, `ambiguity`, `not_covered`, and `unbound`, inputs its scope named that the conversation does not hold |
| `divergence` | `conclusion`, `verdict` (`reached`, `contradicted`, `not_observed`, `unknown` or `not_applicable`), `target`, `event`, `criteria` or `whole`, `asserted` for a verdict of absence, `note`, `cite` |
| `rollback` | `conclusion`, `target`, `event`, `criteria`, none for the whole result, `note`, `cite` |
| `finding` | `target`, `source`, the stage that recorded it or `asz`, `conclusion` and `cause` for a divergence finding, `asserted` for a cause of absence, `at`, the step or input where the problem happened, its `ref`, the `problem`, its `explanation` and `improvement`, and `cite` |
| `label` | `target`, `talk`, `from` (the first step of a part; absent for a whole talk), `label`, `subtask` (for `misled_by_subtask`, the sub-task whose untrue report the work acted on), `note`, `cite` |
| `subtask` | `id`, the sub-task's stream; `parent`, the main talk or stream holding the step that started it; `started_by`; `outcome`, `report`, `use`, `dispatch`, `work`, `note`, `cite` |
| `stage` | `stage`, `file`, `digest`, `status`, `evaluator` |
| `outcome` | `target`, `statement`, `status`; `level`, `basis`, `met`, `unknown`, `contradicted`, the claims found contradicted, and `criteria` (absent when no achievement stage judged it); `cost`, an amount; `avoidable`, `avoidable_by_subtasks`, `avoidable_cause_unknown` and `not_reviewed`, amounts, absent when no efficiency stage ran; `span_ms`, `avoidable_span_ms`; `class`; `standing`, the delivery events standing at the end; `undone`, the undoings that apply, those that took back a delivery still standing; `score_record` and `score_final`, each from 0 to 1, absent when the target is not scored or cannot be verified; `rework`, what its rounds of rework cost: the calls of every round, each counted once, by the label the stages gave them, each a `work`, for avoidable work its `cause`, whose doing it was, and `cost`, an amount; `corrections`, the inputs that corrected it; `operations`, each an `id` and the target's `share` when its call was shared; `references`, the conclusions whose contradiction or rollback applies to its final score |
| an amount | `usage`; `money`, absent when no call in it was priced; `unpriced` and `unknown`, the calls in it with no price and with no usage; `output_unknown`, the calls in it that never finished, their output not counted. A share of a call is counted, so tokens are rounded |

A `cite` is a list of `{node, ref}`: a node id of the fold at the head, and the landed record the node
stands on, as `{seq, row, block}`. Every value the [conversation judge](../concepts-and-designs/conversation-judge.md)
page lists is the whole vocabulary of `action`, a relation's `kind`, `status`, `level`, `basis`, a
check's `state`, an observation's `status`, an event's `kind` and `artifact`, a round's `trigger` and
`cause`, and `label`.

### Scopes of a cost

| `scope` | Is the cost of |
| --- | --- |
| `conversation` | every call of the conversation |
| `talk` | a main talk, with the child streams it alone started |
| `loose` | the main stream's calls no talk contains |
| `unattributed` | the child streams no single talk started |
| `target` | the talks that serve only this target |
| `shared` | one talk serving several targets, written once per target with its `share` |
| `unassigned` | the talks no target claims, and the main stream's calls no talk contains |
| `work` | one target's work under one `work` label; with `id` and `share` when it is a shared talk's; for avoidable work, `cause`: `main` for the main agent, the id of the sub-task whose doing it was, or `unknown` when the record does not show whether a misleading sub-task's task was right |
| `subtask` | on the close file, a sub-task a main talk gave, with every sub-task under it, and the `work` its decision named. A view over calls counted in other scopes, never added to a total |

The ledger's `talk`, `loose` and `unattributed` costs add up to its `conversation` cost exactly. In
the close file, `target`, each `shared` cost times its share, `unassigned` and `unattributed` add up to
it too. `usage` holds only the calls whose usage is known; `unknown` counts the rest, whose tokens are
not in it.

### Commit

`digest` covers every line before the commit, the header included, each ending in its newline: the
SHA-256 of those lines, in lowercase hex. `counts` holds the number of each body frame, so a reader
detects a truncated file.

## On disk

```text
<session>/judgments/
  j000001/                          one run
    s1-ledger-<digest12>.sj
    s2-targets-<digest12>.sj
    s3-achievement-<digest12>.sj
    s4-efficiency-<digest12>.sj
    s6-reference-<digest12>.sj      when the run was given a source
    s7-divergence-<digest12>.sj
    s9-close-<digest12>.sj          the run is done once this exists
    g1-regroup-<digest12>.sj        a regroup by a person's goals, made over the closed run
    work/                           what each evaluator was given and said; never sent
```

A run lives in the directory of the session its conversation was assembled from, not in a chain's
directory. A chain's directory is removed to parse a conversation again, and a judgment cannot be
rebuilt from landed files. In the session's directory a run goes wherever the session goes: a
[retirement](storage-root.md#retention) removes it with the session, and a retired session gets no
new run. `metadata_only_after` leaves runs as they are. A stage file is written once and made
read-only, as a landed file is. Its name ends in `.sj`, never in a landed file's `-NNNNNN.sd`, so
nothing that reads landed files takes it for one.

## A regroup

A regroup is made over a closed run from a person's goals, a plain list in their own words. It is not
a stage of the run: it has a file of its own beside the run's, named by its version,
`g<version>-regroup-<digest12>.sj`, and the run's files are never changed. Its header has the stage
`regroup`, `builds_on` the run's targets stage, the evaluator whose one call placed the targets, with
what it consumed as a stage's header has it, and `labels`: the label file's `file`, its `sha256` and
the regroup's `version`. It is `partial` when parts of the answer were left out, its note saying
which, and `failed` when the call gave no answer, with only its `label_text`. Its frames, in this
order:

| Frame | Fields |
| --- | --- |
| `label_text` | `from` and `text`: the label file as the person gave it, cut as a source's text is, the frames in order the file, so the comparison can be made again after the file changed or was removed |
| `label_goal` | one of the person's goals, in their order: `id` such as `P1`, `statement`, its words, `members`, the targets that serve it, `main`, `judge`, the goals of the judge its targets are in, `class`, `worked` when the conversation worked on it, `note`, and `cite`, the person's inputs that asked for it |
| `placement` | one per target of the run: `target`, `goal`, the person's goal it serves, absent for none, `unknown` when the answer left it out or nothing shows its goal, `note`, and `cite`, the person's inputs that show its goal |
| `agreement` | `goals`, how many the person gave; `classes`, the count of each; `extra`, the targets that serve none of them; `unknown`, the targets not placed |

The classes, most severe first: `missed`, `split`, `merged`, `unknown`, `absent`, `agree`.

## Reading it

A reader rejects a stage file, rather than reading it, when any of these is true:

- The first line is not a header, a second header appears, or a line follows the commit.
- A line does not decode, or its frame type is unknown.
- The header's schema is not `sj/1`. It lacks `conversation`, `session`, `head` or `evaluator`. Its
  `run` is 0. Its `stage` or `status` is not one this page lists. It lacks `started` or `finished`.
- The commit's digest does not match the lines before it, or its counts do not match the frames.
- A `stage` frame's digest is not a SHA-256 in lowercase hex.
- The last line has no newline.

A key a frame does not define is passed over, so a later writer can add keys without stopping an
earlier reader. A run is complete to a reader once its close file is there. The
[exporter](otlp.md#judge-runs) sends a run only then.
