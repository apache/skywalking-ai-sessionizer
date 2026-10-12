# A chain written by the previous parser

This root was written by asz at commit `6fbf0ae`: `asz scenario build tests/scenarios/fixture.yaml
--format sd`, then `asz parse`. It holds the four landed files of one session and the round that
parse wrote, `r000001-4388f7a5d811.sf`. Nothing else is kept: the index is rebuilt from the landed
files, and the chain head is found by listing the rounds.

That round still carries the node attributes later builds stopped writing: a segment's `state`,
`gates_unmet` and `committable`, the session's `conversation` and `title_from`, an epoch's
`records`, a talk's `loops`, `runs` and `trigger`, a run's `trigger`, a call's `fragments` and a
tool's `name`. Keep these files unchanged. `round_upgrade_test.go` continues the chain with the
current parser and checks that the conversation comes out as a fresh parse would build it.
