## Task: which of the person's goals each target serves

The person who had this conversation listed their goals for it, in their own words, below. The targets are the outcomes this run found the person asked for. Decide, for every target, which of the person's goals it serves, and for every goal, whether the conversation worked on it.

A target serves a goal when its result is part of what the goal asks for, or a step toward it, as the person's requests show. Read the inputs that set the target, with `inputs` and `step`, and decide by what the person asked, never by shared words, files or nearness in time alone.

For each target, give one entry in `targets`:

- `target`: its id, such as `t3`.
- `goal`: the id of the person's goal it serves, such as `P2`, or empty when it serves none of them, such as work the agent needed along the way.
- `cite`: the person's inputs that show it serves that goal, such as the input that asked for it. A target put in a goal with no input of the person that shows it is not placed.
- `note`: why, in one sentence.

For each of the person's goals, give one entry in `goals`:

- `goal`: its id.
- `worked`: whether the conversation worked on it at all, even when no target serves it.
- `cite`: the person's inputs that asked for it, when it was worked on.
- `note`: why, in one sentence.

Every target and every goal appears exactly once. A target serves at most one goal: when it serves several, give the one its result is mostly for, and say so in `note`.
