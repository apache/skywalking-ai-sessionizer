# Fixture: Claude Code local files

A synthetic corpus reproducing the shapes found in a real Claude Code corpus.
It is synthetic by necessity - real transcripts contain a person's actual work -
but every shape here was observed in real data, and each is cited at the
assertion that depends on it.

Session ids must be UUID-shaped or discovery rejects them, so the case number
lives in the first segment. The project directory name carries the case name,
since a slugified working directory is unconstrained.

| Case | Session id | Covers |
| --- | --- | --- |
| **01** | `00000001-0000-4000-8000-000000000001` | a complete conversation: human turn, injected context, a provider call split across three fragments, a tool call and result, a server-side tool id, a subagent spawn with its async return, a client-fabricated `<synthetic>` record, and an explicit compaction boundary |
| **02** | `00000002-0000-4000-8000-00000000000A` | an uppercase session id — joins must be byte-exact, never case-folded |

Case 01 also includes, deliberately:

- `memory/` — a sibling directory inside a project dir that is **not** a session
- `not-a-uuid.jsonl` — a file whose name is not a session id
- a workflow run: child stream, journal, manifest, and a **script filed under a
  different project directory**, which is why discovery groups by session id
  across directories
- a child stream whose usage is a streaming partial, unlike the main stream's
  repeated final usage

A third case, a session that **grows across collection rounds**, is built
programmatically in `builder_test.go` rather than committed here: it has to be
written incrementally to exercise cursors resuming between rounds.
