# Scenarios

A scenario is a short YAML description of a conversation, in the model's own words: a person's
input, a call with its fragments, a tool and its result, a child agent, a context reset. `asz
scenario build` turns it into the input a real session leaves behind, so the ordinary commands
collect and parse it. `asz scenario check` runs it as a test.

The `.sf` side is never mocked. A scenario produces evidence; the ordinary parser produces the
rounds. That is what makes a scenario both a fixture generator and a conformance test: built as
the runtime's own files and collected through its adapter, or landed directly as Session Data, it
must fold to the same conversation.

## Build

```sh
asz scenario build FILE --format claude-code --out DIR      # the runtime's files, under DIR/_source
asz scenario build FILE --format sd --out DIR               # Session Data landed into DIR, dialect mock/1
asz collect -once -config DIR/asz.yaml                      # for a runtime format: the real adapter lands it
asz parse -config DIR/asz.yaml                              # the real parser writes the rounds
asz view -config DIR/asz.yaml                               # or verify, or push
```

`build` writes the input and `DIR/asz.yaml`, whose storage root is `DIR` and whose adapter source
is `DIR/_source`, then stops. It refuses to change a `DIR/asz.yaml` that says something else.

| Flag | Meaning |
| --- | --- |
| `--format` | `claude-code` or `sd` |
| `--out` | the directory; it ends as a storage root |
| `--at` | the base time, RFC 3339, or `now` (default) |
| `--scale` | multiplies every delta; `60` turns a scenario typed in seconds into minutes |
| `--interval` | overrides the scenario's interval |
| `--repeat N` | N sessions end to end on the clock, each with its own id |
| `--through NAME` | only the steps up to the checkpoint NAME |

With a fixed `--at`, every file is identical on every run.

## The scenario

```yaml
session: mock-build-and-check       # optional; default derived from the steps
title: build and check              # optional
interval: 1s                        # the gap between steps, in every stream
steps:
  - input: run the build            # a person's message: opens a run and a talk
  - inject: {type: skill_listing, text: "skills: 1"}
    after: 100ms                    # this step's own delta since the previous one
  - call:                           # one provider call, in fragments
      thinking: unavailable         # a reasoning part with no text; any other value is the text
      text: Building now.
      tool: {id: t1, name: Bash, input: {command: make build}, result: {text: build succeeded, after: 800ms}}
      usage: {in: 2, out: 50, cache_read: 900, cache_write: 100}
    checkpoint: built               # a point a test can stop at
  - call:
      agent:                        # a child agent: the call, its acknowledgement, the child's own stream
        name: checker
        prompt: check the tests
        after: 1s                   # the child's first record, relative to the call
        steps: [{call: {text: Tests pass.}}]
        notify: true                # the runtime reports the child finished: a second run
  - call: {text: Build passed and tests are green.}
  - error: "API Error: Connection lost mid-response."   # an assistant-role message the client made
  - reset: {summary: "Summary: the build was run and checked."}
```

Every step is exactly one of these, plus an optional `after` and `checkpoint`:

| Step | What it writes |
| --- | --- |
| `input` | a person's message; `trigger: external`; opens a run and a talk |
| `queued: {text, mode}` | input that exists only as a queued attachment; `mode` is `prompt` or `task-notification` |
| `inject: {type, text}` | material the harness put into context, of any type |
| `call` | fragments in this order: `thinking`, `text`, then one of `tool`, `agent`, `skill`, `workflow`; `usage` on every fragment; the last carries the stop reason |
| `result: {of, text, after, failed, string}` | a tool result arriving on its own, for a tool whose call gave none |
| `error` | a synthetic message |
| `reset: {summary}` | a context reset and its summary, the summary timestamped before the boundary as the runtime writes it |
| `replay: N` | the last N main-stream records re-emitted with their run rewritten |
| `system: {subtype, fields}` | a system record of any subtype |

A `tool` without a `result` is an unfinished tool. A `skill: {name, agent, steps}` is a fork whose
child is announced only in the parent's result. A `workflow: {name, children: [{name, prompt,
steps}]}` starts children as one batch, with a journal, a manifest and a script. Ids are stable and
the same in every format: the step's position names its records, a tool is `tool/<id>`, a run is
`<step>-cycle`, a child stream's id is derived from its name.

## Check

```sh
asz scenario check FILE [--format claude-code|sd|all] [--out DIR] [--at TIME] [--scale FACTOR]
```

For each format, and at each checkpoint in order, `check` builds through the checkpoint, collects
when the format needs it, parses, and compares the fold with the expectation file beside the
scenario, `NAME.expect.yaml`. At the end it runs the properties every chain must have, and with
`all`, the default, it compares the formats' folds with each other. It exits non-zero on any
failure and keeps its directory when one is given.

```yaml
checkpoints:
  built:                                  # named in the scenario
    rounds: 1
    kinds: {tool: 1, llm.call: 1}
    nodes:
      tool/t1: {refs: 2, attrs: {result: available, timing: unavailable}}
  final:                                  # the end of the scenario
    rounds: 2
    delta: true                           # the round written here is a delta
    talks_on: {main: 2, checker: 1}       # a stream may be named by its scenario name
    runs_in: {talk/main/s1-cycle: 2}
    relations: {starts: 1, reports: 1}
    unresolved: {open: 0, resolved: 0}
    unresolved_kinds: {tool_result: none}
    session: {from: +0s, to: +11.1s}      # the session node's range, as deltas from --at
    view: {state: verified, talks: 3, files: 6, first_talk: {label: run the build, runs: 2}}
properties:                               # all on unless set false
  reproducible: true
  fold_equals_parse: true
  immutable_rounds: true
  bundle: true
  header_matches_fold: true
  records_well_formed: true
  repack_keeps_structure: true
  recollect_idempotent: true              # runtime formats only
  every_line_a_record: true               # runtime formats only
  discovery_ignores_noise: true           # runtime formats only
  cross_format: true
  records_match: true
parse:
  max_round_bytes: 0                      # a parse setting, when the scenario needs one
```

Only what is written is checked. The properties are: two parses of the same landed files write
the same rounds; folding every round equals one full parse; rounds verify, link and are not
writable; the landed files and rounds are self-sufficient without index and state; the head
round's header says what the fold holds; a parse with no new evidence writes nothing; every landed
record carries only the fields the format states a purpose for; a repack under the smallest budget
keeps every record and the whole structure; and, for a runtime format, a second collect lands
nothing, every source line becomes one landed record, and discovery passes over the noise the
writer plants beside the session. Across formats, the folds must agree, and so must the landed
records themselves, field by field: the runtime's adapter and the sd writer must land the same
evidence from the same scenario, which is what makes a scenario a conformance test for an adapter.

The project's own tests are scenarios under `tests/scenarios/`, one property of assembly each,
run in both formats by `go test ./tests/`.
