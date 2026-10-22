## Task: the targets the person set

Find what the person asked the agent to achieve.

A **target** is an outcome the person asked for, with criteria you could check. "Fix the login timeout and keep the public API unchanged" is a target. "Authentication" is a topic, not a target. A question the person wants answered is a target too, and its criterion is a correct answer.

"This run" lists the inputs you answer for, each with the last thing the agent said before it, so that a short input such as "yes", "go" or "the second one" can be read. It also lists the targets found so far by earlier parts of this run, if any. Earlier inputs are context: read them with `inputs` when one of yours cannot be understood without them. Read `talks` when you need to know what a talk did. Read a talk or a step only when an input cannot be understood without it.

For every input you answer for, record what it did to the targets. It can do more than one thing:

- `create`: it asks for a new outcome.
- `refine`: it adds, changes or drops a criterion of an existing target.
- `steer`: it directs how to go on without changing the outcome, such as "continue", "retry", "use the other library".
- `accept`: the person accepts the result.
- `correct`: the person says the result is wrong or incomplete.
- `cancel`: the person drops the target.
- `none`: it asks for nothing, such as thanks.
- `not_from_person`: it is text the runtime or another program recorded as input, such as a notice or a scheduled message.
- `unknown`: you cannot tell.

An action names a target found so far by its id, such as `t3`, or a new target of your answer by its key, such as `n1`.

Then give the targets your inputs set or changed, one entry each:

- A new target has a `key` of your own, such as `n1`, never the id of a target found so far, and an empty `id`. asz gives it its id. Give its `statement`, `criteria`, `status`, `talks` and `cite`.
- A change to a target found so far has its `id` and an empty `key`, and only what changed: a new `statement`, `criteria` to add, `drop_criteria` by criterion id, `status`, `replaced_by`, `talks` to add, `cite`. Leave a field empty when it does not change. Nothing is removed from a target: a criterion the person dropped goes in `drop_criteria`.

The fields:

- `statement`: the outcome, in one sentence, as the person meant it by the end of your inputs.
- `criteria`: what would show it was achieved, taken from what the person said or plainly required. Do not add expectations the person did not state, such as "nothing else breaks" or "the rest still works": the record can rarely show them, and an unshown criterion lowers the level of work that did what was asked.
- `status` as of your last input: `open`, `closed` (the work for it ended with a result delivered or accepted), `cancelled`, or `replaced`. When the person changed what they wanted so much that it is a different outcome, keep the old target as `replaced` and name the new one in `replaced_by`.
- `talks`: the talks whose work serves the target: the talks of its inputs, and the talks that went on with it after a steer such as "go" or "retry". A talk can serve two targets when its inputs act on both. Do not list sub-tasks: a sub-task belongs to the targets of the talk that started it.
- `cite`: the input nodes that set and changed it.

Prefer one target for one deliverable. A follow-up that fixes, extends or polishes the same deliverable refines it rather than creating another. Two deliverables the person treats as separate are two targets, even when they come in one input. When an input acts on a target set before the inputs you can see, set it as new and cite the earlier input that set it.

Every input you answer for appears exactly once in your `inputs` answer, by its node id. An input you did not read is `unknown`.
