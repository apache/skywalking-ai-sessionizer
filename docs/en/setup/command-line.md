# Command Line

```text
asz sources [-config FILE]                    list the sessions found, and their files
asz collect [-config FILE] [-once]            collect, assemble and send, every interval
asz server [-config FILE] [ADDR]              collect, and serve the page
asz view [-config FILE] [ADDR]                serve the page for data already collected; changes no collected data
asz push [-config FILE]                       send everything not sent yet, then exit
asz parse [-config FILE] [SESSION]            assemble conversations from collected data
asz conversation [-config FILE] [-json|-yaml] ID   show one conversation
asz show [-config FILE] SESSION ID            print the collected record behind an id
asz verify [-config FILE] [SESSION]           check collected data against its digests
asz repack [-config FILE] DEST [SESSION]      copy the data into DEST, cut to the configured file size
asz index [-config FILE] [SESSION]            report the lookup index
asz measure [-config FILE] [-pricing FILE] [-json] ID   what a conversation's model calls consumed
asz judge [-config FILE] [-round N] [-from T -to T | -from-id ID -to-id ID] [-evaluator NAME | -plan ...] ID   judge a conversation; -list and -show read runs
asz judge [-config FILE] -remote-conversation REMOTE/ID[:TAG] ...   judge a pulled conversation
asz judge [-config FILE] -pull -remote-conversation REMOTE/ID[:latest] ...   pull its head, then judge it as a new run
asz pull [-config FILE] REMOTE/ID ...           copy conversations from another asz, to judge and read here
asz pull [-config FILE] -list [-remote NAME] ...   list what the remotes hold
asz token create [-file FILE] NAME            make a token for an asz that serves pulls
asz glossary                                  what Claude Code calls each thing asz names
asz scenario build|check FILE ...             build or check a test scenario
asz version                                   print the version
```

A `SESSION` is a session's identifier, as `asz sources` shows it, and an `ID` a conversation's, as
the page shows it, whatever runtime recorded them. In `asz show SESSION ID` the `ID` is a record's
or a tool use's identifier instead, and `-from-id` and `-to-id` take talks' identifiers. A
`REMOTE/ID` names a conversation on a remote as `asz pull -list` shows it.
[Command Line Internals](../guides/command-line-internals.md) explains the reports in detail.

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

Serves the conversations at `ADDR`, `127.0.0.1:8787` by default. It collects nothing and changes no
collected data. It writes a lease while it reads a pulled snapshot. When its root is a pulled
snapshot that a stopped pull left half published, it deletes that unfinished snapshot and records
its tag removed. When it answers pulls, it keeps the pull server's records under `_pull/`. Use it
for data collected elsewhere or by another process. The list is at `/`, and one conversation at
`/c/{id}`. The Evidence tab shows the collected record behind a step, and the Prompt tab a model
call's request and response, when [provider bodies](claude-code-provider-bodies.md) were collected.
The page withholds from every reader what [`view.hide`](configuration.md#view) names, the system
prompt and the tool schemas a runtime sent, unless `view.hide_override` lets a request's `hide`
parameter name another set. `server` does the same. `conversation` withholds nothing. See [Withholding](../formats/asz-view.md#withholding).
The list also holds the conversations `asz pull` copied into the root, each named with
`[remote, NAME]` before its title. Each request takes a lease on the pulled snapshot it reads, and
lets it go when it is answered.

With [`remote_server.tokens_file`](configuration.md#remote_server) set, `view` and `server` need a
token on every request, and also answer `asz pull` from other machines, keeping the pull server's
records under `_pull/`. A root that is itself a pulled snapshot answers no pulls. See
[Pull from Another asz](remote-pull.md).

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

It also counts the parts that were too large to land, which are not a problem.

## repack

Copies the storage root, or one session, into `DEST`, a new or empty directory outside the root,
cutting its files at `max_delta_bytes`, and assembles the conversations there. Use it after
changing `max_delta_bytes`. A record larger than the new budget is carried whole, in a file of its
own, over the budget: repack never leaves anything out.

## index

Reports what the lookup index of a session holds. The index can be deleted at any time; the next
collection rebuilds it.

## measure

Prints what one conversation's model calls consumed: tokens by type, money where the price file prices
a call, and the costliest talks with the child agents each started. It calls no model. `-json` prints
the ledger as one object: `total`, every `owners` cost, every `main_talks` cost with its child agents,
`unattributed`, `prices` and `dialect`. See [Prices](judge.md#prices).

## judge

Judges one conversation at its current head, or at round `-round N`: the targets the person set,
whether each was achieved, how each sub-task went, and what each piece of work was, each stage given to
an evaluator. `-from` and `-to`, or `-from-id` and `-to-id`, judge a window of whole main talks. `-list`
lists a conversation's runs and `-show N` prints one. See [Judge a Conversation](judge.md).

`-remote-conversation REMOTE/ID` judges a conversation `asz pull` copied here, in place of `ID`: the
snapshot with `:TAG`, or the newest unexpired one without a tag or with `:latest`. `-pull` pulls its
head first and judges it as a new run: it takes no fixed tag, and no `-list`, `-show` or `-regroup`.
See [Pull from Another asz](remote-pull.md#judge-a-pulled-conversation).

## pull

Copies conversations from the asz servers named under [`remotes`](configuration.md#remotes), each as
`REMOTE/ID`, into the storage root, and prints each copy's snapshot. `-list` lists what the remotes
hold instead, newest activity first. It exits 0 when everything it was asked succeeded, 1 when a pull,
a remote's listing or a copy here failed, and 2 when its arguments are wrong. When one conversation
fails, the next goes on. When a remote itself fails, such as a refused token or certificate, its
remaining pulls stop, and the other remotes go on. A conversation a remote's listing could not read
is said, and does not fail the command. See [Pull from Another asz](remote-pull.md).

## token create

Makes a token for an asz that serves pulls, and prints it. With `-file`, adds its line to that tokens
file. Without, prints the line to add.

## glossary

Prints each name asz uses, what Claude Code calls it, and where Claude Code records it.

## scenario

Builds a test session from a scenario file, or checks one against its expectations. See
[Scenarios](../guides/scenario.md).

## version

Prints the version, the Go version and the platform.
