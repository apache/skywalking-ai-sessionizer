# Welcome

**This is the official documentation of SkyWalking AI Sessionizer. You're welcome to join us.**

SkyWalking AI Sessionizer assembles the fragmented telemetry of a long-lived AI agent into one
durable conversation. It reads the files an agent runtime already writes, lands them in a format no
runtime owns, and builds the conversation structure as a chain of immutable, verifiable rounds.

- [Concepts and Designs](en/concepts-and-designs/overview.md). Start here to understand what
  the project is, the conversation model it assembles, and the evidence rules every page follows.
- [Setup](en/setup/quick-start.md). Build the binary, collect this machine's history, assemble it
  and read it. Configuration, the command line and the container image.
- [Data Formats](en/formats/storage-root.md). What is on disk: the storage root, Session Data
  (`.sd`) and Session Flow (`.sf`). These are public. A third-party adapter produces the first and
  a consumer reads the second.
- [Adapters](en/adapters/claude-code.md). How a specific agent runtime maps onto the model, and
  what it can and cannot supply.
- [Guides](en/guides/contributing.md). Contributing and releasing.
- [Changelog](en/changes/changes-0.2.0.md). What the version in development contains, and one page
  per released version.

We're always looking for help improving our documentation and codebase, so please don't hesitate to
[file an issue](https://github.com/apache/skywalking-ai-sessionizer/issues/new) if you see any problem.
Or better yet, submit your own contribution to help make it better.
