# Quick Start

This page takes a machine that has used Claude Code and turns its history into conversations you
can read. Nothing in Claude Code changes: no plugin, no hook script, no environment variable. The
collector reads files that are already on disk, so it also works on history written before it was
installed.

## Build

Go 1.25 or later. The module has one dependency, a YAML parser.

```sh
git clone https://github.com/apache/skywalking-ai-sessionizer.git
cd skywalking-ai-sessionizer
make build          # -> ./bin/asz
./bin/asz version
```

Every command reads `asz.yaml` from the working directory when no `-config` flag is given. The
file at the repository root is the default configuration with every value written out, so the
commands below work as they are. See [Configuration](configuration.md) to change where data comes
from or goes.

## See what is there

```sh
./bin/asz sources
```

```text
source root: /Users/me/.claude/projects
filtered   : 20 session(s) excluded by config

SESSION                               DIRS  STREAMS  META  JOURNAL  MANIFEST
0438c73b-2367-4ed5-9de3-13ef9a17ed01  2     132      131   14       14
04b56e12-fae2-4413-a7c1-f18911a1463f  1     1        0     0        0
```

One row per session: how many directories its files are spread across, how many execution
streams it has (the main transcript plus one per child agent), and how many child-agent sidecars,
workflow journals and manifests were found. The source directory is resolved the way Claude Code
resolves it: `CLAUDE_CONFIG_DIR`, then `XDG_CONFIG_HOME`, then `~/.claude`. Sessions under
`/private/tmp` are Claude Code's own helper agents and are excluded by default.

## Land the history

```sh
./bin/asz collect -once
```

```text
[17:14:09] sessions=44 sources=5867 landed=5867 records=359292 bytes=1.0GB indexed=359292 gone=0 conflicts=0 busy=0 pending=0 errors=0 (2m39.473s)
```

Every source file is read from its cursor onward and written into the storage root as Session
Data, then indexed. The line above is one machine's first pass, measured on 2026-09-03: 44
sessions, 5,867 source files, 1.0 GB of records, in 2 minutes 39 seconds. Later passes read only
what is new; the next pass on the same machine landed one source in 933 ms. A pass with `pending`
or `errors` above zero did not collect everything, and the command exits non-zero so a script can
tell. Re-running is safe: landing is idempotent by design.

## Assemble

```sh
./bin/asz parse
```

```text
SESSION                               ROUND  SEQ   NODES  RELS  UNRES  TALKS  RUNS  STEPS  TOOLS        CHILDREN
0438c73b-2367-4ed5-9de3-13ef9a17ed01  1      305   17249  703   0      357    445   11473  5620/5620    131/131
04b56e12-fae2-4413-a7c1-f18911a1463f  1      1     409    11    0      11     11    277    146/146      0/0
04c6e9de-3b81-4106-a08b-e75259c33ca4  1      1     5      0     0      0      0     2      0/0          0/0
…
44 round(s) written
```

Each session becomes one conversation, and each parse appends one round to its chain when
something changed. `TOOLS` and `CHILDREN` show how many tool calls and child-agent launches were
joined to their results, out of how many exist. A second run with nothing new writes no rounds.

## Read

```sh
./bin/asz view
```

Open `http://127.0.0.1:8787`. The list page shows every conversation; a conversation page shows
its talks, its execution streams, the flow on a time axis, and the evidence behind every step.

On a machine with Claude Code, `asz view` alone is enough. It runs the collector and the parser
itself, every 5 seconds by default, and the list page shows when the data was last refreshed and
when it will be next. The two steps above are worth running once to see what each does.

## Check

```sh
./bin/asz verify
```

```text
checked 44 session(s), 5867 stream(s), 359292 records
checked 44 conversation chain(s), 44 round(s)
all landed data is contiguous and matches its digests
```

Every landed file is checked against its digest and every chain against its own commit digests.
This needs no source files and no collector, so it also works on a storage root copied from
another machine.

## Where it went

Everything is under the storage root, `./data` by default. Landed files are write-once, the index
is derived and disposable, and the round chain is append-only. Deleting the directory starts over.
See [Storage Root](../formats/storage-root.md).
