## Task: whether each target was achieved

The targets are listed below, each with its criteria, each criterion with an id, and the talks that serve it. For each target, decide whether it was achieved as of the head of the conversation.

"This run" lists the talks of the targets you judge, as the tools show them. They are an overview, not evidence: a row says what a talk was about, never what it proved. Before you decide a criterion, open the step that shows it, with `talk` and `step`: a tool result, a recorded change, or the reply that delivered an answer, whose id a talk row gives as `reply_node`. Cite that step. An input shows what the person asked, never that it was done. Then read the last replies, and the steps that prove or disprove the outcome: failed tools, test and build commands and their results, file changes, and what the person said afterwards. A file change shows in two ways. A recorder may have captured it: read it with `changes` and `diff`. A tool result may show it: a diff, a status, a test that ran against it, or a read of the file after the edit. Both are evidence. When the card says no change was recorded, that says only that no recorder ran, never that nothing changed: read the tool results, and say `not_captured` only when nothing in the record shows the file.

A target's sub-tasks are part of its evidence. Find them with `subtasks` and the target's talk ids. What a sub-agent did counts when its own steps show it, such as a test it ran and the result that came back. That a sub-agent reported it did something is a claim, not evidence of it.

For every criterion of the target, by its id, give one state with citations:

- `met`: the record shows it done, all of what it names, by work for this target. Work done for something else, such as a fix the agent needed along the way, meets none of its criteria.
- `not_met`: the record shows it failed, was not done, was done for only part of what it names, or the work stopped before it. Say in `note` what part is missing: links added for 43 of 61 people do not meet "add the links".
- `unknown`: you could not tell, or could not read the evidence.
- `not_captured`: the evidence would be in something the record did not capture.

Every criterion of the target appears exactly once. A `met` or `not_met` holds only when what you cite shows it: a tool step that returned a result, or the person's own later input. The input that asked for the target shows what was asked, never that it was done, and a tool call whose result never came shows nothing. The agent's replies, reasoning and model calls show only what it said, with one exception: when an answer is what a criterion asks for, such as the answer to a question or an explanation asked for within a change, the reply shows whether it was delivered, or which requested part is missing. Set `delivered` on every such criterion and cite the reply; the basis `delivered_answer` alone does not mark them. A file, a document or a change is never shown by a reply: cite the step that wrote or read it. A criterion that cites nothing that shows it is taken as `unknown`. asz takes the level from your states: `achieved` when every criterion is `met`; `partly` when some are `met` and some are not; `not_achieved` when none is `met` and at least one is `not_met`; `cannot_verify` otherwise, and when the target has no criterion.

The basis of the outcome:

- `person_accepted`: the person's own later reaction accepts the result. Cite that input. It accepts what the person was shown: a step the person asked for that the record shows was never done stays `not_met`, even when the person approved the rest.
- `observed_result`: a tool result shows it, such as test output, a command's output or a recorded change. Cite that tool step.
- `delivered_answer`: the target was an answer or an explanation, and the reply delivers it. That the reply holds an answer is evidence it was delivered; whether what the answer says is true is a separate claim, checked like any other. Cite the reply.
- `agent_claim_only`: only the agent says so.
- `no_evidence`: nothing shows it either way.

A basis that nothing you cite shows is weakened to `agent_claim_only` or `no_evidence`.

Then check the agent's claims about the outcome in its replies, such as "the tests pass", "the PR is open" or "fixed". For a delivered answer, list each statement it makes about the code, the system or the record that the record can check, such as which path calls which or why a value changed, as a claim of its own, and check it. An answer delivered but wrong is not achieved. Check each against the tool results: `supported`, `contradicted`, `no_evidence` or `not_captured`. A later correction by the person, such as "it still fails", counts against an earlier claim. Never take the agent's words as their own evidence: a claim supported or contradicted only by the agent's words is `no_evidence`. A target with a contradicted claim is not achieved, whatever its criteria say.

Give one answer per target, for every target listed. Put what the outcome rests on in `note`, in one or two sentences.
