# Command Line

```text
asz sources [-config FILE]                    list the sessions found, and their files
asz collect [-config FILE] [-once]            collect, assemble and send, every interval
asz server [-config FILE] [ADDR]              collect, and serve the page
asz view [-config FILE] [ADDR]                serve the page for data already collected; only reads
asz push [-config FILE]                       send everything not sent yet, then exit
asz parse [-config FILE] [SESSION]            assemble conversations from collected data
asz conversation [-config FILE] [-json|-yaml] ID   show one conversation
asz show [-config FILE] SESSION ID            print the collected record behind an id
asz verify [-config FILE] [SESSION]           check collected data against its digests
asz repack [-config FILE] DEST [SESSION]      copy the data into DEST, cut to the configured file size
asz index [-config FILE] [SESSION]            report the lookup index
asz glossary                                  what Claude Code calls each thing asz names
asz scenario build|check FILE ...             build or check a test scenario
asz version                                   print the version
```

A `SESSION` or `ID` is a Claude Code session id. [Command Line Internals](../guides/command-line-internals.md)
explains the reports in detail.

## Flags

| Flag | Meaning |
| --- | --- |
| `-config FILE` | The configuration file. Default: `./asz.yaml` when present, else the defaults. |
| `-once` | Collect once, instead of every interval. `collect` then exits; `server` goes on serving. |
| `-terms MODE` | In `asz conversation`, name things as asz does (`unified`, the default), as Claude Code does (`native`), or `both`. |

## sources

Lists the sessions asz finds in Claude Code's directory, how many files and streams each has, and
how many the session filters left out. Nothing is collected.

## collect

Collects what is new, assembles the conversations that changed, and sends them when
`export.otlp.endpoint` is set. It repeats every `interval`, or once with `-once`. Each collection
that finds something prints one line:

```text
[10:12:03] refreshed: sessions=44 landed=12 records=8130 rounds=3 pushed=15 metrics=2 (2.4s)
```

Errors are listed below the line. With `-once`, any error makes the command exit non-zero.

## server

`collect` and `view` in one process, at `ADDR`, `127.0.0.1:8787` by default. This is what to run on
your own machine. The list page shows when data was last collected.

## view

Serves the conversations at `ADDR`, `127.0.0.1:8787` by default, and changes nothing. Use it for
data collected elsewhere or by another process. The list is at `/`, and one conversation at
`/c/{id}`. The Evidence tab shows the collected record behind a step, and the Prompt tab a model
call's request and response, when [provider bodies](claude-code-provider-bodies.md) were collected.

## push

Sends every collected file and round not sent yet to `export.otlp.endpoint`, then exits. Use it for
data collected elsewhere; `collect` and `server` send as they go. A file that fails to send is sent
on the next run. See [Export over OpenTelemetry](export-otlp.md).

## parse

Assembles every session, or one, and prints a row per session. `collect` and `server` already do
this; run it on data that arrived without them.

## conversation

Prints a summary of one conversation. With `-json` or `-yaml`, prints the whole
[asz.view](../formats/asz-view.md) document, the one the page draws:

```sh
asz conversation -json 0438c73b-2367-4ed5-9de3-13ef9a17ed01 > conversation.json
```

## show

Prints the collected record that carries a record id or a tool-use id: what a step is based on.

## verify

Checks every collected file and every round against its digest, and that no data is missing. It
needs only the storage root, and exits non-zero when anything is wrong:

```text
checked 44 session(s), 5867 stream(s), 359292 records
checked 44 conversation chain(s), 44 round(s)
all landed data is contiguous and matches its digests
```

## repack

Copies the storage root, or one session, into the new or empty directory `DEST`, with no file
larger than `max_delta_bytes`, and assembles the conversations there. Use it after changing
`max_delta_bytes`.

## index

Reports what the lookup index of a session holds. The index can be deleted at any time; the next
collection rebuilds it.

## glossary

Prints each name asz uses, what Claude Code calls it, and where Claude Code records it.

## scenario

Builds a test session from a scenario file, or checks one against its expectations. See
[Scenarios](../guides/scenario.md).

## version

Prints the version, the Go version and the platform.
