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
  which. A request carries up to `batch_bytes`, 20 MiB by default, and a larger file goes alone.
  The protobuf encoding is written in the project, so the module still has one dependency.

## Assembly

- A round is cut at `parse.max_round_bytes`, 2 MiB by default, the same budget as a landed file,
  because a round travels whole as one log record. The parser narrows the round's input window
  until the round fits and leaves the rest of the evidence to the next round; `asz parse` and the
  refresh loop keep going until the chain reaches the index. Measured on the real corpus, the
  largest round was 17 MB and 8 of 589 were over 4 MiB before the budget.

## Read

- The page reads Session Data and Session Flow and nothing else. It took every record's time from
  the index; it now takes it from the record, so a root that arrives with only its landed files
  and its rounds renders in full, and the index is assembly's alone.

## Formats

- Session Data's reader and writer can carry a record as the bytes of its line, unchanged, which
  is what a repack and a receiver on a wire need so file digests still match.
