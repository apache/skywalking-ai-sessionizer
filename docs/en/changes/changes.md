# Changes in 0.1.0

> In development, not yet released. This page collects the changes for the next version. At
> release time it is renamed to `changes-0.1.0.md`, this note is removed, and a new page starts.

The first release. It establishes the conversation model, one adapter that fills it from local
Claude Code files, the two public data formats, and a local page to read the result.

## Model

- The Unified Conversation Model: a conversation is the durable ownership boundary whose identity is
  supplied and never inferred; an execution stream is the ordered continuity boundary within which
  model-message continuity is evaluated. Every word with a fixed meaning is in the glossary.
- Evidence discipline throughout: nothing is inferred to fill a missing identifier, a reference that
  cannot be resolved is carried as data, and content is always labelled with how much of the
  original is present.

## Collection

- The `claude-code-local` adapter reads Claude Code's files from this machine: main transcripts,
  child-agent transcripts and their sidecars, and workflow journals, manifests and scripts. It needs
  no plugin, no hook and no configuration in Claude Code, and it works on history written before it
  was installed.
- Landed files are write-once, sequenced per session, and land before their cursor commits. A
  derived index of identifiers and roles is built while landing and can be rebuilt from the landed
  files alone.
- Session filters by working directory, with Claude Code's own helper agents under `/private/tmp`
  excluded by default.
- A source reached through another filesystem, such as a bind mount into a container or a copied
  tree, resumes where the cursor left off when its bytes are unchanged. Only truncated or rewritten
  bytes are a conflict.

## Assembly

- The eight-stage pipeline: remove duplicates, partition streams, group provider calls, join
  tools, join child agents, cut context epochs, build the conversation, and publish the delta.
- Conversation structure is published as an append-only chain of immutable rounds. A round carries
  no wall-clock time, so the same evidence reproduces the same bytes and digest. Absence in a later
  round means unchanged; removal is an explicit tombstone.

## Formats

- Session Data, `.sd`, schema `sd/1`: the landed record, with content broken into parts named for
  what they are and every part labelled with its state and original size.
- Session Flow, `.sf`, schema `sf/1`: the round chain, self-verifying through commit digests and
  bound to its evidence through a chained input digest.

## Read

- `asz view` serves a list of conversations and one page per conversation with its talks, its
  execution streams, the flow on a time axis and the evidence behind every step. With the local
  adapter it also runs the collector and the parser on the configured interval and shows the last
  and next refresh.
- `asz verify` checks landed data and round chains without the source files.

## Tooling

- One binary, `asz`, with `sources`, `collect`, `index`, `show`, `parse`, `conversation`, `view`,
  `glossary`, `verify` and `version`.
- A default configuration file, `asz.yaml`, read from the working directory, with every value
  written out and held to the compiled defaults by a test.
- A Linux container image for amd64 and arm64 on the GitHub container registry, tagged by version
  once a release is released on GitHub after the vote.
- Binary packages for macOS on Apple silicon and Intel, Linux on x86-64 and ARM 64, and Windows on
  x86-64, each with `LICENSE` and `NOTICE`, built by `make release` for the vote and published on
  the downloads page. On Windows the session lock is an exclusive open rather than a file lock,
  and a file's identity is its volume serial and file index rather than an inode.
- The collector side and the assembly and read side meet only at the storage root. A test fails
  when either imports the other, so a later split into two binaries is packaging, not a refactor.
