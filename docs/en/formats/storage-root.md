# Storage Root

The storage root is a directory. Everything the project keeps is under it, in two halves: what was
collected, one directory per session, and what was assembled, one directory per conversation.
Beside them sit the metrics spool and the export state, which belong to the root rather than to a
session.

```text
<root>/
  <session-id>/
    session.state                        next_seq, liveness (always unknown), last scan
    .lock                                one collector per session
    streams/
      main/
        transcript.cursor                how far the source has been read
        transcript-<collected-at>-<seq>.sd
        changes.cursor                   the plugin's change records for the stream, when
        changes-<collected-at>-<seq>.sd  the asz Claude Code plugin is installed
      <agent-id>/                        one directory per child agent
        transcript.cursor
        transcript-<collected-at>-<seq>.sd
        meta.cursor
        meta-<collected-at>-<seq>.sd
    runs/
      <run-id>/                          one directory per workflow run
        journal.cursor · journal-…sd
        manifest.cursor · manifest-…sd
        script.cursor · script-…sd
    index/
      entries.bin                        the derived index
      index.state                        how far the index reaches
  _conversations/
    <conversation-id>/
      conversation.state                 the head pointer, outside every digest
      .lock                              one parser per chain
      rounds/
        r000001-<digest>.sf
        r000002-<digest>.sf
  _metrics/                              the metrics spool, when an adapter produces metrics
    spool.state                          the next number for a received request
    metrics.state                        what the local derivation has counted
    .lock
    metrics-<received-at>-<seq>-otlp.pb  one request the receiver adapter was sent
    metrics-<session-id>-<seq>-local.pb  the points derived from the session's landed file <seq>
  push.state                             what asz push has sent, with each file's digest
  _export/
    .lock                                one pusher per root
```

Child streams are flat siblings of `main`, keyed by agent id. The storage path deliberately does
not mirror the source tree: a path must not encode a relationship the pipeline is supposed to
derive. [Export over OpenTelemetry](../setup/export-otlp.md) says how the metrics spool fills and
what `push.state` records.

`liveness` in `session.state` is always `unknown`. asz has no check for whether a session is
still running.

## Landed files

A landed file is [Session Data](session-data.md): one header line and then one record per source
record. It is written once to a temporary name, synced, made read-only, and only then renamed into
place. So a file under its final name is read-only from the moment it appears. It is never appended
to and never rewritten. A source that keeps growing produces more files, each starting where the
previous cursor stopped.

A landed file is named `<kind>-<collected-at>-<seq>.sd`. `<collected-at>` is the UTC time the
session's pass started, with nanoseconds and no separators, such as `20260831T091204.481523000Z`.
Names in this form sort in time order as plain text. They hold no `:`, which Windows does not allow
in a file name. Every file that one pass lands for a session carries the same stamp. `<seq>` is
zero-padded to at least six digits. A reader orders files by the sequence, never by the stamp.

The sequence number in the name is issued per session and is monotonic across every stream in it.
That is what lets assembly track progress with one watermark instead of one entry per stream, and
it is why a session is collected single-threaded under its lock. The filesystem is the authority on
the counter: on start, a collector raises its counter above the highest sequence already on disk,
so a crash between writing a file and saving state cannot reissue a number.

A file lands before its cursor is committed. A crash between the two lands the same records twice
in the next pass. The reverse order would lose data. The cursor would already point past records
that never landed, and no later pass would read them. The index keeps the first copy of each
record id and marks later copies as duplicates, which assembly skips. A record with no id has
nothing to compare, so both of its copies stay in the index. `asz verify` counts records whose
source line had already landed as re-landed, and does not fail on them.

A file is cut once. The collector cuts at `max_delta_bytes`, 2 MiB by default, and a round then
addresses every record by file and line and binds itself to the file digests, so re-cutting a file
that a round references would break every reference. A change of budget applies to new files
only. To bring an existing root under a new budget, `asz repack DEST` re-cuts every file into a
new root, keeping each record's bytes and order, carries the cursors over, and builds the chains
again on the new files, which describe the same conversation at new positions.

## Cursors

Each source has one cursor, beside the files it tracks, named `<kind>.cursor`. The dot matters.
Landed files are named `<kind>-<collected-at>-<seq>.sd`, so a cursor named with a hyphen would share
their prefix, and a scan for one kind's files would take the cursor for data.

A cursor belongs to one source rather than to the session, because a session can hold hundreds of
sources. Measured on 2026-09-11 on one machine's storage root of 60 Claude Code sessions, the
largest session has 616 streams and 1,309 cursors, 614,788 bytes in all. One file per session would
mean rewriting all of that to record that one source moved. A cursor is rewritten only when its own
source changed. The other numbers in this section come from the same root.

A cursor is plain text, one key and value per line, replaced atomically. There are two kinds.

### Append cursors

Sources that only grow are tracked by position: transcripts, workflow journals, and the plugin's
change records.

```text
schema        1
kind          append
source        -Users-me-github-project/221ac729-….jsonl
dev           16777232
ino           287890584
offset        62230163
ord           17108
last_uuid     573afdf5-…
tail_sha256   98ebccc0…
size          62230163
mtime         1787283075
last_seq      35
updated_at    2026-09-06T15:02:49.496762Z
state         active
```

| Field | What it is for |
| --- | --- |
| `source` | the source path, relative to the adapter's root, with forward slashes |
| `dev`, `ino` | the device and the inode the file had when it was read, or on Windows its volume serial number and file index. Only `ino` is compared, and it is what this page calls the file's identity. A change means the file was replaced, or is being reached another way. The bytes decide which, see below. |
| `offset` | the bytes consumed, always at the end of a line, and where the next read starts. Compared with the file's size, it says whether anything is new. A size below it means the file was cut short. |
| `ord` | the last source line consumed, so line numbers continue without counting again from byte 0 |
| `last_uuid` | the id of the last record that had one. For diagnosis only: many lines carry no id, and nothing compares it. |
| `tail_sha256` | the SHA-256 of the 1 MiB before `offset`. A mismatch means bytes already read were rewritten. |
| `size`, `mtime` | the size and modification time last seen. Recorded, never compared. |
| `last_seq` | the sequence of the last file this source landed |
| `state` | `active`, `source_gone` or `conflict` |

Every pass takes each append source through the same steps:

1. The file is gone. The cursor becomes `source_gone`, and the landed files stay. This is normal:
   Claude Code prunes its transcripts, and the landed files outlive them.
2. The file is shorter than `offset`. It was cut short, and the source is a `conflict`.
3. The size equals `offset` and the identity is unchanged. Nothing is new, and nothing more is
   read. A change is judged by the size, never by `mtime`, so clock skew and timestamp granularity
   cannot hide a change or invent one.
4. Otherwise the 1 MiB before `offset` is digested. If it differs from `tail_sha256`, the source is
   a `conflict`.
5. The read starts at `offset`, takes at most
   [`max_delta_bytes`](../setup/configuration.md#collector), and stops at the last complete
   newline. An unterminated last line is a write in progress. It is left for a later pass, so a
   landed record is always a whole source line. A single line longer than the budget is read to
   its end, so the source cannot get stuck behind it.
6. Each line is converted to a record. A line that is not valid JSON does not stop collection. It
   lands as an `unknown` part that keeps its bytes.
7. The file lands. Then the cursor is saved with the new `offset`, `ord` and `tail_sha256`.
8. While complete lines remain, the pass reads the next window, up to 65 windows from one source.
   A source that still has data after that counts as pending. A watching collector carries on at
   its next pass, and a single pass of `asz collect` exits non-zero. The limit stops one source
   that grows faster than it is read from keeping every other source waiting.

A changed `ino` is not a conflict by itself. The same file reached through another file system
has a new inode: a bind mount into a container, a restored backup, a copied source tree. So the
bytes decide. If the 1 MiB before `offset` still matches, collection continues and the cursor
takes the new `dev` and `ino`. If it does not match, the source is a `conflict`. A cursor with no
tail digest cannot be checked this way, so for it a new `ino` is a conflict. The rule comes from a
failure: when the inode alone decided, every source collected on a host was a conflict when the
same root was collected inside a container.

A change of `dev` alone is never noticed. `dev` is saved with the cursor and never read back.
When nothing lands and `ino` is unchanged, the cursor is not saved, so the `dev` it holds can be
out of date.

The window is 1 MiB rather than the whole consumed prefix, because the whole prefix would be read
again on every pass that finds new data. In the same root, the largest transcript has 62,230,163
bytes consumed. The window catches a change to any byte inside it. A rewrite further back than
1 MiB is not caught. A record can be longer than the window, so the window can cover only part of
the last record: 22 of the 404,956 records from growing sources are over 1 MiB, the largest
4,629,074 bytes.

A conflict is kept. The cursor is saved with `state conflict`, every later pass skips the source,
and nothing in asz clears it. Collection stops because an `offset` into bytes that changed no
longer means what it meant.

The pass that finds the conflict names the file and the reason on standard error. Every later pass
reports only how many sources are in conflict, not which ones. A watching collector prints that
count after each pass, and a single pass of `asz collect` exits non-zero. To find the sources, look
for cursors whose `state` is `conflict`.

All of this rests on transcripts being append-only, and that is observed, not proven. None of the
6,080 cursors in the same root is in conflict.

### Snapshot cursors

Sources that are rewritten whole are tracked by a digest of their content: child-agent sidecars,
workflow manifests and workflow scripts. A snapshot cursor carries `content_sha256` in place of the
position fields. Every pass reads the whole file, trims trailing newlines and digests it. When the
digest differs from `content_sha256`, the whole file lands as a new file of one record, and the
cursor takes the new digest. An empty file lands nothing.

So a change to such a source becomes a new immutable version, never an edit, and a run directory
can hold several manifest files, one per version. This kind exists for the workflow manifest. It is
rewritten as its run goes on, and its final version carries the run's `status`, `durationMs`,
`totalTokens` and `result`. A sidecar uses the same cursor. In the same root, each of 2,782 child
streams holds one sidecar version, and each of 153 runs holds one manifest version.

## The index

`index/` holds identifiers and roles, never text: which record is in which stream, which call a
fragment belongs to, which tool use a result answers, which child a launch started. Assembly reads
this and never the payloads; the page does not read it at all, and takes every time from the
record itself. It is derived and disposable. Delete it and the next collect or parse
rebuilds it from the landed files; a schema change discards it rather than migrating it.

`index/index.state` records `indexed_seq`, the landed sequence the index covers, with its schema
and its counts of entries, blocks and strings. Cursors commit per source, but the index is written
once per session, at the end of its pass. A crash between the two leaves the index behind what
landed, and nothing would land again to fill the gap. So the next collect compares `indexed_seq`
with the highest sequence on disk and indexes the landed files above it again. Parse never reads
past `indexed_seq`, so a round cannot cite a record the index does not hold.

## The chain

`_conversations/<id>/rounds/` holds [Session Flow](session-flow.md): an append-only chain of
immutable rounds. The conversation is the fold of every round; there is no other state. The file
name carries the round number and the first twelve characters of the round's digest.
`conversation.state` is a cache of the head. It is rebuilt by listing the directory, because a
crash between publishing a round and saving state must not lose the round.

Publishing a round reads the head and then writes the next round. `.lock` lets one parser do that
at a time. Without it, two parsers could both read round N and both write a round N+1. The digest
is part of the file name, so the two files would not collide, and the chain would fork with no
error. A second parser gets a lock error and writes nothing. Under the lock, a round that does not
follow the head is refused, and a round file is created only when no file of that name exists, then
made read-only. A test in `tests/chain` runs four parsers at once on one session and requires
exactly one round.

## What travels

A storage root is complete on its own. A copy of it can be listed, verified, re-parsed and read on
another machine with no source files and no collector. Session Data is already converted: its
records name identifiers by role, not by any runtime's fields. So the index rebuilds from the
landed files with no adapter and no runtime-specific code, and the chain head is recovered from the
rounds directory. `asz verify` checks every landed file against its digest and every round against
its commit digest without touching the source.

For the same reason, the index, assembly and the chain hold no code for any one runtime. Supporting
another runtime needs an adapter that writes Session Data, and nothing on the assembling side. The
mock dialect the tests use is one: it writes Session Data directly, and the same code assembles it.

## Size

Measured on 2026-09-03 on one machine's storage root of 48 Claude Code sessions:

| | Files | Size | Against the source |
| --- | --- | --- | --- |
| landed `.sd` | 6,615 | 800 MB | describes 1.2 GB of source records |
| rounds `.sf` | 97 | 105 MB | 8.9% |
| index | 48 | 52 MB | 4.5% |
| cursors and state | 6,766 | 3 MB | |

The landed data is smaller than its source because a record keeps its content and its provenance,
not its envelope. Of the transcript content, 550 MB is carried verbatim; 64,667 reasoning parts are
marked unavailable because Claude Code stores only a signature for them, and each record says so.
The store had also outlived its source: it held 48 sessions where the source directory still had
44.

## Retention

The session directory is the unit of retention: removing it purges everything collected for that
session. Removing `index/` costs a rebuild. Removing a conversation's rounds loses the structure
only; a new parse rebuilds it from the landed files, and because a round carries no wall-clock
time, the same landed range and the same parser and policy versions reproduce the same bytes and
the same digest.

Remove a session directory whole, never only some of its landed files. A round names records by
landed sequence and row, so a round whose files are gone points at records nobody can read.
`asz verify` reports each such round with each missing sequence, and exits non-zero. The
[asz.view](asz-view.md) document is still served. Its `summary.state` is `incomplete`, and
`summary.problems` has one line for each missing file in each round. A node that stood on a missing
record has no text and no content state.
