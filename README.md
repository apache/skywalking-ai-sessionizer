# SkyWalking AI Sessionizer

**Conversation-level observability, measurement, and export for long-lived AI agents.**

> **Status:** pre-alpha. The conversation model and the Claude Code data mapping are defined and
> evidence-backed, and Phase 1 collection is implemented. The assembler, export and preview are not.

SkyWalking AI Sessionizer assembles fragmented agent telemetry into one durable conversation structure. It
preserves sessions as source provenance, keeps parent and child-agent execution lineages separate,
measures model-message continuity, and projects the same committed snapshot into storage, export and
a local preview.

## Why

Agent runtimes record a single user-visible conversation as many unrelated artifacts — transcripts,
traces, logs, provider request bodies, tool events, subagent metadata. That conversation can outlive
one process, reactivate after a long idle period, and contain several concurrent agent lineages.

Trace-level inspection alone cannot answer: what did the whole conversation do; which input, model
response, tool result or child-agent result led to the next model call; did that call preserve the
previous message history or start a new context; which agents contributed; and what should be
measured, stored and exported as one unit.

## Model

```text
Conversation                          durable identity · ownership boundary
 └ Segment                            activity window · the COMMIT unit
    └ Session                         observed source provenance
       ├ ExecutionStream  main        ordered parent-agent lineage
       │  └ Context epoch × N         model-context lifetime
       │     └ Talk × N               one readable input → run → output
       │        └ Run → Step × N
       └ ExecutionStream  child × N   independent context per child agent
```

Two boundaries carry the design. **Conversation** is the durable aggregation and ownership boundary,
and its identity is supplied — never inferred from a person, an account, or timestamp proximity.
**ExecutionStream** is the ordered continuity boundary; model-message continuity is evaluated within
one stream and one context epoch, never across them.

See the [Unified Conversation Model](docs/en/concepts-and-designs/unified-conversation-model.md).

## Evidence discipline

Nothing is presented as observed unless it was observed.

Every claim carries a qualification (`observed_replayable`, `observed_report_only`, `proposed`,
`unavailable`) and every correlation carries a resolution state (`exact_unique`, `exact_ambiguous`,
`strong_inference`, `unresolved`, `conflict`). An exact identifier with several candidates stays
ambiguous — the assembler never silently chooses one. Where a runtime cannot supply something, the
adapter reports it as `unavailable` rather than approximating it.

## Adapters

| Runtime | Status | Collection |
| --- | --- | --- |
| [Claude Code](docs/en/adapters/claude-code.md) | collection implemented | local files — no configuration required, and it works on history that already exists |
| Codex | planned | — |
| LangChain / LangGraph | planned | — |

## Quick start

```sh
make build                 # builds ./bin/asz
./bin/asz sources          # list discovered sessions and their sources
./bin/asz collect -once    # land everything currently on disk
```

## Documentation

Official documentation lives in [`docs/`](docs/) and is indexed by
[`docs/menu.yml`](docs/menu.yml).

[`design-notes/`](design-notes/) holds working engineering notes — measurements, corrections and open
questions produced while designing against real runtime data. They are deliberately unpolished and
are **not** part of the published documentation.

## Contributing

Early contributions should focus on schemas, privacy-safe fixtures, deterministic assembly,
qualification rules and golden tests. Please avoid adding inferred identities or causal edges that
cannot retain their source evidence and resolution state.

## License

[Apache License 2.0](LICENSE).

## Docker

CI publishes a multi-platform image to GHCR on every push to `main` and on
every `v*` tag. It carries the `asz` binary only. Mount a storage root at
`/asz/data`; by default the container serves the page on port 8787.

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest
```

Any `asz` command runs the same way: put it after the image name.
`make docker` builds the image locally.
