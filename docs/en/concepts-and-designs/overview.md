# Overview

SkyWalking AI Sessionizer is conversation-level observability, measurement and export for long-lived AI
agents. It assembles fragmented agent telemetry — transcripts, traces, logs, provider request bodies,
tool events and subagent metadata — into one durable conversation structure.

## The problem

Agent runtimes record a single user-visible conversation as many unrelated artifacts. That
conversation can outlive one process, reactivate after a long idle period, and contain several
concurrent agent execution lineages. Trace-level inspection cannot answer:

- What did the whole conversation do?
- Which input, model response, tool result or child-agent result led to the next model call?
- Did that call preserve the previous message history, or start a new context?
- Which sessions and agents contributed to the same conversation?
- What should be measured, stored, previewed and exported as one unit?

## The approach

SkyWalking AI Sessionizer fixes two boundaries and derives everything else from them:

- **Conversation** is the durable aggregation and ownership boundary. Its identity is supplied by the
  application or adapter — never inferred from a person, an account, or timestamp proximity.
- **ExecutionStream** is the ordered continuity boundary. Model-message continuity is evaluated within
  one stream and one context epoch, never across them.

See the [Unified Conversation Model](unified-conversation-model.md) for the full structure.

## Evidence discipline

The project's central rule is that **nothing is presented as observed unless it was observed**.

Every material claim, statistic, diagram edge and rendered annotation carries a qualification, and
every correlation between two records carries a resolution state — `exact_unique`,
`exact_ambiguous`, `strong_inference`, `unresolved` or `conflict`. An exact identifier with several
candidates stays ambiguous; the assembler must never silently choose one.

Where a runtime cannot supply something, the adapter reports it as `unavailable` rather than
approximating it. A missing model input, an unrecorded parent, or an unobservable causal link is
never manufactured to make a tree look complete.

## What is implemented

The conversation model and the Claude Code data mapping are defined and evidence-backed. These
parts are implemented:

- **Collection, the derived index and assembly** into a round chain. `asz collect` runs them as one
  pipeline. When `export.otlp.endpoint` is set, it also sends at the end of every pass.
- **The local page.** [`asz view`](../setup/command-line.md#view) serves the assembled
  conversations and only reads. [`asz server`](../setup/command-line.md#server) runs the pipeline
  and the page in one process.
- **Export over OpenTelemetry.** [`asz push`](../setup/export-otlp.md) sends the landed files and
  the rounds to an OpenTelemetry logs receiver. It also sends the token metric when an adapter
  produces it.
- **One conversation as one document.** [asz.view](../formats/asz-view.md) is what the page draws
  and what `asz conversation -json` prints.
- **A storage root that travels on its own.** A copy of it can be verified, re-parsed and read on
  another machine with no source files. See [What travels](../formats/storage-root.md#what-travels).

A static export is not built.

## Non-goals

- Grouping work by human or principal identity.
- Inventing missing model input, sessions, parentage or causal links.
- Re-exporting the original discrete traces and logs as the final product.
- Acting as a general observability backend or product UI.
