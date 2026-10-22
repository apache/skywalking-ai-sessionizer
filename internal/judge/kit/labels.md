## The labels of work

Each piece of work gets one label:

- `useful`: work that moved the target forward.
- `exploration`: looking around that a careful engineer would also do, even when it found nothing.
- `verification`: checking the work, such as tests, builds and reviews.
- `avoidable_attempt`: an approach that failed for a reason visible at the time, or a detour away from the target.
- `repeated`: the same action again with nothing new learned, while the earlier result was still in the agent's context, such as running an unchanged failing command again. After a compaction or a rewind, use `repeated` only when the summary or the history that was kept shows the agent still had the result; when you cannot tell, do not use it.
- `redundant_delegation`: a sub-task whose work duplicated work already done or already under way when it was started, as the record showed at that moment. A sub-task started as a reasonable check, or in parallel with others to save time, is not redundant because its result went unused.
- `repair_own_mistake`: fixing a defect in the agent's own work after it said that work was done, whoever found the defect: the person, a review, a CI run or a later test. Also fixing a mistake that went against something visible when it was made, such as an instruction, an error already shown or a fact the agent had read. Cite the step that made the defect and the step that found it. A defect the agent's own checks found before it delivered the work is normal work: label it `useful` or `verification`.
- `rework_requirement_change`: work done again because the person changed what they wanted. This is the person's cost, not the agent's.
- `misdirected_delegation`: a sub-task's own work when the task it was given was wrong when it was given: too wide or too narrow, against what the person asked, a command or arguments built wrong, or instructions already out of date. The sub-task did what it was told, so this is the cost of the agent that gave the task.
- `misled_by_subtask`: the asking agent's work done on a sub-task's report that was not true to the sub-task's own work, from the step that acted on it to the step that found it wrong. Name the sub-task in `subtask`. Whose cost it is follows from that sub-task's decision: its own if its task was right and its work went wrong, the asking agent's if the task it was given was wrong.

Judge with what was known at that moment, not with hindsight. Failing is not waste by itself: exploration that rules out an option is useful. Use `useful`, `exploration` or `verification` unless the record shows the work was avoidable.

A row's `failed_steps` and `repeats` point where avoidable work is most often found: tool steps that failed, and tool steps that ran the same tool with the same input more than once. Open them first, and decide for each whether it was avoidable. A long piece of work labeled useful as a whole often hides an avoidable part: label that part on its own, from its first step.
