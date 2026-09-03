---
name: collect
description: Collect this machine's local Claude Code conversation data with the asz collector. Resolves the local Claude data directory (honouring CLAUDE_CONFIG_DIR and XDG_CONFIG_HOME), builds the binary, lists what is discoverable, and lands everything into the storage root. Use when asked to collect, backfill, re-collect, or inspect local Claude Code sessions, or to check what the collector can see.
user-invocable: true
---

# Collect local Claude Code data

Runs the `claude-code-local` adapter over this machine's own Claude Code files.
It reads only what is already on disk — nothing needs to be enabled in Claude
Code, and it works on history that predates the collector.

## 1. Resolve the source directory

The location is **not** fixed. Resolve in this order and report which one won:

```sh
if   [ -n "$CLAUDE_CONFIG_DIR" ];  then SRC="$CLAUDE_CONFIG_DIR/projects"
elif [ -n "$XDG_CONFIG_HOME" ];    then SRC="$XDG_CONFIG_HOME/claude/projects"
else                                    SRC="$HOME/.claude/projects"
fi
ls -d "$SRC" && du -sh "$SRC"
```

`asz` performs the same resolution internally, so normally you do not pass
`source_root` at all. Set it only when collecting from a copy or a mounted corpus.

## 2. Build

```sh
make build          # -> ./bin/asz
```

## 3. See what is discoverable before collecting

```sh
./bin/asz sources
```

Reports one row per session with its stream, meta, journal and manifest counts,
then a footer naming any session whose files span more than one source
directory. Read it for two things:

- **`filtered: N session(s) excluded by config`** — the default config excludes
  `/private/tmp/**` (Claude Code's own agent scratchpads). If a session you
  expect is missing, this is why.
- **sessions spanning several directories** — normal. A child agent that ran in
  a different working directory is filed under that directory instead.

## 4. Collect

Default storage root is `./data`, relative to the working directory.

```sh
./bin/asz collect -once          # single pass; the backfill path
./bin/asz collect                # watch mode, 5s interval
```

To land somewhere else, write a config and pass it:

```yaml
# collect.yaml
storage:
  root: /tmp/asz-data
adapters:
  - name: claude-code-local
    enabled: true
    collector:
      mode: once
```

```sh
./bin/asz collect -config collect.yaml
```

## 5. Read the result line

```
sessions=61 sources=5831 landed=5831 records=364051 bytes=1.0GB
gone=0 conflicts=0 busy=0 pending=0 errors=0 (2m16s)
```

| Field | Meaning |
| --- | --- |
| `landed` | sources that produced new data this pass |
| `gone` | source deleted since last pass — **normal**, Claude Code prunes transcripts, and our landed copy outlives them |
| `conflicts` | a source was rotated, truncated or rewritten behind the cursor; collection stopped for it |
| `busy` | session skipped because another collector holds its lock |
| `pending` | a source still had data when the per-pass drain limit was reached |

**`pending` or `errors` above zero means the pass did not collect everything**,
and `asz collect` exits non-zero. A clean pass is `pending=0 errors=0`.

Re-running is cheap and safe: cursors make a steady-state pass ~1s over several
thousand sources, and landing is idempotent by design.

## What lands where

```
<root>/<session-id>/
    session.state                                 next_seq, liveness
    streams/main/       cursor · transcript-<ts>-<seq>.jsonl
    streams/<agent-id>/ cursor · transcript-… · meta-cursor · meta-…
    journal/<wf-id>/    cursor · journal-<ts>-<seq>.jsonl
    manifest/<run-id>/  cursor · manifest-<ts>-<seq>.jsonl
```

Landed files are **write-once and read-only**. Each is a header line plus one
line per source record, with the source bytes preserved verbatim in `payload`.

Purging a session is `rm -rf <root>/<session-id>/`.

## If something looks wrong

- **A session is missing** — check the `filtered` line first, then `asz sources`.
- **`conflicts` is non-zero** — the cursor for that source is now sticky. Inspect
  `state` in its `cursor` file; the source was rotated, truncated, or rewritten
  behind the position we had already consumed.
- **Nothing is discovered** — confirm step 1 resolved to a directory that exists
  and contains per-project subdirectories.

Do not hand-edit anything under the storage root. Cursors and landed files are
consistent only as written.
