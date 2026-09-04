# Storage Root

The storage root is a directory. Everything the project keeps is under it, in two halves: what was
collected, one directory per session, and what was assembled, one directory per conversation.

```text
<root>/
  <session-id>/
    session.state                        next_seq, liveness, last scan
    .lock                                one collector per session
    streams/
      main/
        transcript.cursor                how far the source has been read
        transcript-<collected-at>-<seq>.sd
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
      index.state
  _conversations/
    <conversation-id>/
      conversation.state                 the head pointer, outside every digest
      .lock
      rounds/
        r000001-<digest>.sf
        r000002-<digest>.sf
```

Child streams are flat siblings of `main`, keyed by agent id. The storage path deliberately does
not mirror the source tree: a path must not encode a relationship the pipeline is supposed to
derive.

## Landed files

A landed file is [Session Data](session-data.md): one header line and then one record per source
record. It is written once, to a temporary name, synced, renamed into place and made read-only.
It is never appended to and never rewritten. A source that keeps growing produces more files, each
starting where the previous cursor stopped.

The sequence number in the name is issued per session and is monotonic across every stream in it.
That is what lets assembly track progress with one watermark instead of one entry per stream, and
it is why a session is collected single-threaded under its lock. The filesystem is the authority on
the counter: on start, a collector raises its counter above the highest sequence already on disk,
so a crash between writing a file and saving state cannot reissue a number.

A file lands before its cursor is committed. A crash between the two lands the same records twice
in the next pass, and assembly removes the duplicate. The reverse order would lose data.

A file is cut once. The collector cuts at `max_delta_bytes`, 4 MiB by default, and a round then
addresses every record by file and line and binds itself to the file digests, so re-cutting a file
that a round references would break every reference. A change of budget applies to new files
only. To bring an existing root under a new budget, `asz repack DEST` re-cuts every file into a
new root, keeping each record's bytes and order, carries the cursors over, and builds the chains
again on the new files, which describe the same conversation at new positions.

## The index

`index/` holds identifiers and roles, never text: which record is in which stream, which call a
fragment belongs to, which tool use a result answers, which child a launch started. Assembly reads
this and never the payloads; the page does not read it at all, and takes every time from the
record itself. It is derived and disposable. Delete it and the next collect or parse
rebuilds it from the landed files; a schema change discards it rather than migrating it.

## The chain

`_conversations/<id>/rounds/` holds [Session Flow](session-flow.md): an append-only chain of
immutable rounds. The conversation is the fold of every round; there is no other state. The file
name carries the round number and the first twelve characters of the round's digest.
`conversation.state` is a cache of the head. It is rebuilt by listing the directory, because a
crash between publishing a round and saving state must not lose the round.

## What travels

A storage root is complete on its own. A copy of it can be listed, verified, re-parsed and read on
another machine with no source files and no collector: the landed files name the adapter that
wrote them, so the index rebuilds from them alone, and the chain head is recovered from the
rounds directory. `asz verify` checks every landed file against its digest and every round against
its commit digest without touching the source.

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
time, the same landed range and the same parser version reproduce the same bytes and the same
digest.
