# Changes in 0.2.0

> In development, not yet released. `tools/release.sh prepare 0.2.0` removes this note.

## Collection

- The default landing budget, `max_delta_bytes`, is 2 MiB rather than 8 MiB. A landed file
  travels whole as one log record, and 2 MiB keeps a record small next to what a receiver
  accepts while a session of the measured corpus still lands in a few hundred files. A single
  source record larger than the budget is landed whole, and the largest measured is 4.5 MB.
- `asz repack DEST` re-cuts the landed files of a root into a new root under the configured
  budget, keeping every record's bytes and order, carrying the cursors over, and building the
  chains again on the new files. A landed file is never rewritten in place.

## Export

- `asz push` sends every landed file and every round to an OpenTelemetry logs receiver over
  OTLP/HTTP, one log record per file with the file's bytes as the body, so a receiver stores the
  file as landed and checks its digest at once. Records name the sender with `telemetry.sdk.name`,
  the format and its version, the file, its kind, its digest and its line count, and the session
  and sequence a round's reference resolves against. Each file is sent once; `push.state` records
  which. A request carries up to `batch_bytes`, 8 MiB by default, under the 10 MiB the OAP accepts
  over HTTP, and a larger file goes alone.
  The service is `export.otlp.service_name`, or the runtime the adapter reads, `Claude Code`, so a
  receiver lists conversations by the agent that produced them; the layer is `AI_AGENT`, spelled
  as the OAP spells a layer. A round's record also carries `asz.conversation.title` and the
  fold's counts, and every record is stamped with a time inside the session's range, so a receiver
  lists conversations and bounds its reads without decoding a body.
  The protobuf encoding is written in the project, so the module still has one dependency. Each
  record also carries `asz.from_time` and `asz.through_time`, the record time range of the file,
  so a receiver can place a file in time without decoding it, and a round's record carries
  `asz.session.from_time` and `asz.session.through_time`, the session's range as of that round.

## Assembly

- A round is cut at `parse.max_round_bytes`, 2 MiB by default, the same budget as a landed file,
  because a round travels whole as one log record. The parser narrows the round's input window
  until the round fits and leaves the rest of the evidence to the next round; `asz parse` and the
  refresh loop keep going until the chain reaches the index. Measured on the real corpus, the
  largest round was 17 MB and 8 of 589 were over 4 MiB before the budget.

- A round's header carries `from_time` and `through_time`, the earliest and the latest record time
  among the landed files it consumed, and the `session` node carries the same pair for the whole
  conversation so far, which makes its `from_time` when the session began. The header repeats the
  session's pair as `session_from_time` and `session_through_time`, so a reader of the header alone
  has it, and its `title` and the fold's counts of talks, steps, streams, segments and open
  unresolved references, so a list of conversations needs no fold. All are evidence, so they
  reproduce with the round and sit inside its digest.

## Scenarios

- `asz scenario build` turns a short YAML scenario, written in the model's own words, into the
  input a session leaves behind: Claude Code's own files for the adapter to collect, or Session
  Data landed directly under a new `mock/1` dialect. `asz scenario check` runs a scenario as a
  test against an expectation file, in both formats and at every checkpoint, and checks the
  properties every chain must have. The rounds are never mocked; the ordinary parser writes them.
  The project's own assembly tests are now scenarios under `tests/scenarios/`, and the Go
  transcript builder they used became the `claude-code` writer. Every scenario is also pushed to
  an OTLP receiver in the test and checked against the export page, one file per request, both
  formats and all six file kinds, then rebuilt from the wire and verified.

## Read

- `asz.view`, version 1.0, is one conversation rebuilt from its rounds and its landed files as one
  document: every talk as a tree with the text, usage and flags its records carry, the streams, the
  segments, the relations, and the rounds and files it was built from, each verified, with a gap or
  a failed digest written into the document rather than returned as an error. Package
  `pkg/sessionview` defines and owns the shape and `asz view` serves it at `/api/c/{id}/view`;
  `asz conversation -json` or `-yaml` prints it. It is never a file. The format page says how each
  view of a conversation is drawn from the document, and carries a complete example generated from
  the fixture scenario, which a test keeps current. The document holds every run and step of the
  fold: what no talk contains sits under `loose`, and a property of every scenario holds the
  document to the whole session, every round and every landed file included, however many rounds
  the session was parsed in. A server holding the same files, such as the SkyWalking OAP, builds the same
  document. See the format page.

- The page reads Session Data and Session Flow and nothing else. It took every record's time from
  the index; it now takes it from the record, so a root that arrives with only its landed files
  and its rounds renders in full, and the index is assembly's alone.

## Formats

- Session Data's reader and writer can carry a record as the bytes of its line, unchanged, which
  is what a repack and a receiver on a wire need so file digests still match.
