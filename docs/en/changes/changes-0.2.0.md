# Changes in 0.2.0

> In development, not yet released. `tools/release.sh prepare 0.2.0` removes this note.

## Collection

- The default landing budget, `max_delta_bytes`, is 4 MiB rather than 8 MiB, so a landed file
  fits in one message under the default limit of an OpenTelemetry Collector. Measured on a
  corpus of 6,970 files, 38 exceeded 4 MiB under the old default and none exceeded 8 MiB.
- `asz repack DEST` re-cuts the landed files of a root into a new root under the configured
  budget, keeping every record's bytes and order, carrying the cursors over, and building the
  chains again on the new files. A landed file is never rewritten in place.

## Export

- `asz push` sends every landed file and every round to an OpenTelemetry logs receiver over
  OTLP/HTTP, one log record per line with the line's bytes unchanged, so a receiver can write the
  files back byte for byte. Records name the sender with `telemetry.sdk.name`, the format and its
  version, the file, the line and its kind. Each file is sent once; `push.state` records which. The
  protobuf encoding is written in the project, so the module still has one dependency.

## Read

- The page reads Session Data and Session Flow and nothing else. It took every record's time from
  the index; it now takes it from the record, so a root that arrives with only its landed files
  and its rounds renders in full, and the index is assembly's alone.

## Formats

- Session Data's reader and writer can carry a record as the bytes of its line, unchanged, which
  is what a repack and a receiver on a wire need so file digests still match.
